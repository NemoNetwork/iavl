package iavl

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/cosmos/iavl/v2/metrics"
	"github.com/dustin/go-humanize"
	"github.com/emicklei/dot"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var log = zlog.Output(zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: time.Stamp,
})

type Tree struct {
	version        int64
	root           *Node
	metrics        *metrics.TreeMetrics
	kv             *KvDB
	sql            *SqliteDb
	lastCheckpoint int64
	orphans        []*nodeDiff
	cache          *NodeCache
	pool           *NodePool
	checkpointer   *checkpointer

	workingBytes uint64
	workingSize  int64

	// options
	maxWorkingSize uint64

	branches []*Node
	leaves   []*Node
	sequence uint32

	// debug
	// when emitDotGraphs is set to true, the tree will emit a dot graph after each Set, Remove and rotate operation
	emitDotGraphs bool
	lastDotGraph  *dot.Graph
	dotGraphs     []*dot.Graph
}

func NewTree(sql *SqliteDb, pool *NodePool) *Tree {
	tree := &Tree{
		sql:            sql,
		pool:           pool,
		cache:          NewNodeCache(),
		metrics:        &metrics.TreeMetrics{},
		maxWorkingSize: 2 * 1024 * 1024 * 1024,
		lastDotGraph:   dot.NewGraph(dot.Directed),
	}
	return tree
}

func (tree *Tree) SetKV(kv *KvDB) {
	tree.kv = kv
}

func (tree *Tree) LoadVersion(version int64) error {
	if tree.sql == nil {
		return fmt.Errorf("sql is nil")
	}

	tree.version = version
	tree.root = nil
	tree.orphans = nil
	tree.workingBytes = 0
	tree.workingSize = 0
	tree.cache.Swap()

	var err error
	tree.root, err = tree.sql.LoadRoot(version)
	if err != nil {
		return err
	}
	// TODO
	tree.lastCheckpoint = version
	return nil
}

func (tree *Tree) LoadVersionKV(version int64) (err error) {
	tree.root, err = tree.kv.getRoot(version)
	tree.version = version
	return err
}

func (tree *Tree) WarmTree() error {
	var i int
	start := time.Now()
	log.Info().Msgf("loading tree into memory version=%d", tree.version)
	loadTreeSince := time.Now()
	tree.loadTree(&i, &loadTreeSince, tree.root)
	log.Info().Msgf("loaded %s tree nodes into memory version=%d dur=%s",
		humanize.Comma(int64(i)),
		tree.version, time.Since(start).Round(time.Millisecond))
	err := tree.sql.WarmLeaves()
	if err != nil {
		return err
	}
	return tree.sql.queryReport(5)
}

func (tree *Tree) loadTree(i *int, since *time.Time, node *Node) *Node {
	if node.isLeaf() {
		return nil
	}
	*i++
	if *i%1_000_000 == 0 {
		log.Info().Msgf("loadTree i=%s, r/s=%s",
			humanize.Comma(int64(*i)),
			humanize.Comma(int64(1_000_000/time.Since(*since).Seconds())),
		)
		*since = time.Now()
	}
	// balanced subtree with two leaves, skip 2 queries
	if node.subtreeHeight == 1 || (node.subtreeHeight == 2 && node.size == 3) {
		return node
	}

	node.leftNode = tree.loadTree(i, since, node.left(tree))
	node.rightNode = tree.loadTree(i, since, node.right(tree))
	return node
}

func (tree *Tree) SaveVersionKV() ([]byte, int64, error) {
	tree.version++
	tree.sequence = 0

	var sequence uint32
	tree.deepHash(&sequence, tree.root)

	since := time.Now()
	if tree.version == 1 {
		sort.Slice(tree.branches, func(i, j int) bool {
			return bytes.Compare(tree.branches[i].nodeKey[:], tree.branches[j].nodeKey[:]) < 0
		})
		for i, node := range tree.branches {
			err := tree.kv.setBranch(node)
			if err != nil {
				return nil, tree.version, err
			}
			if i != 0 && i%100_000 == 0 {
				log.Debug().Msgf("setBranch i=%s, wr/s=%s",
					humanize.Comma(int64(i)),
					humanize.Comma(int64(100_000/time.Since(since).Seconds())),
				)
				since = time.Now()
			}
		}
	}

	sort.Slice(tree.leaves, func(i, j int) bool {
		return bytes.Compare(tree.leaves[i].nodeKey[:], tree.leaves[j].nodeKey[:]) < 0
	})
	for i, node := range tree.leaves {
		err := tree.kv.setLeaf(node)
		if err != nil {
			return nil, tree.version, err
		}
		if i != 0 && i%100_000 == 0 {
			log.Debug().Msgf("setLeaf i=%s, wr/s=%s",
				humanize.Comma(int64(i)),
				humanize.Comma(int64(100_000/time.Since(since).Seconds())),
			)
			since = time.Now()
		}
	}

	err := tree.kv.setRoot(tree.root, tree.version)
	if err != nil {
		return nil, tree.version, err
	}

	tree.leaves = nil
	tree.branches = nil
	tree.orphans = nil

	return tree.root.hash, tree.version, nil
}

func (tree *Tree) SaveVersion() ([]byte, int64, error) {
	tree.version++
	tree.sequence = 0

	var sequence uint32
	tree.deepHash(&sequence, tree.root)
	//for _, node := range tree.leaves {
	//	tree.cache.SetThis(node)
	//}

	start := time.Now()
	_, _, err := tree.sql.BatchSet(tree.leaves, true)
	if err != nil {
		return nil, tree.version, err
	}
	dur := time.Since(start)
	tree.metrics.WriteDurations = append(tree.metrics.WriteDurations, dur)
	tree.metrics.WriteTime += dur
	tree.metrics.WriteLeaves += int64(len(tree.leaves))

	if tree.version == 1 {
		// TODO
		// psuedo hacky checkpoint here
		err := tree.sql.NextShard()
		if err != nil {
			return nil, 0, nil
		}

		_, versions, err := tree.sql.BatchSet(tree.branches, false)
		if err != nil {
			return nil, tree.version, err
		}

		err = tree.sql.MapVersions(versions, tree.sql.shardId)
		if err != nil {
			return nil, 0, err
		}

		err = tree.sql.addShardQuery()
		if err != nil {
			return nil, 0, err
		}

		log.Info().Msg("creating leaf index")
		err = tree.sql.write.Exec("CREATE INDEX IF NOT EXISTS leaf_idx ON leaf (version, sequence)")
		if err != nil {
			return nil, tree.version, err
		}
		log.Info().Msg("creating leaf index done")
	}

	tree.leaves = nil
	tree.branches = nil
	tree.orphans = nil

	if tree.sql != nil {
		err = tree.sql.SaveRoot(tree.version, tree.root)
		if err != nil {
			return nil, tree.version, fmt.Errorf("failed to save root: %w", err)
		}
	}

	/*
		if tree.workingBytes > tree.maxWorkingSize {
			log.Info().Msgf("checkpointing tree version=%d", tree.version)
			if err := tree.sqlCheckpoint(); err != nil {
				return nil, tree.version, err
			}

			tree.lastCheckpoint = tree.version
			tree.orphans = nil
			tree.workingBytes = 0
			tree.workingSize = 0
			tree.cache.Swap()

			tree.root.leftNode = nil
			tree.root.rightNode = nil
		}
	*/

	return tree.root.hash, tree.version, nil
}

func (tree *Tree) asyncCheckpoint() error {
	args := &checkpointArgs{
		//delete:  tree.orphans,
		version: tree.version,
	}
	tree.buildCheckpoint(tree.root, args)
	if int64(len(args.set)) != tree.workingSize {
		return fmt.Errorf("set count mismatch; expected=%d, actual=%d", tree.workingSize, len(args.set))
	}
	tree.checkpointer.ch <- args
	return nil
}

func (tree *Tree) sqlCheckpoint() error {
	start := time.Now()
	args := &checkpointArgs{}
	tree.buildCheckpoint(tree.root, args)
	if int64(len(args.set)) != tree.workingSize {
		return fmt.Errorf("set count mismatch; expected=%d, actual=%d", tree.workingSize, len(args.set))
	}

	//var memSize, dbSize uint64
	err := tree.sql.NextShard()
	if err != nil {
		return err
	}

	dbSize, versions, err := tree.sql.BatchSet(args.set, false)
	if err != nil {
		return err
	}

	err = tree.sql.MapVersions(versions, tree.sql.shardId)
	if err != nil {
		return err
	}

	// this will pause async readers and flush the WAL
	//err = tree.sql.resetShardQueries()
	//if err != nil {
	//	return err
	//}

	err = tree.sql.addShardQuery()
	if err != nil {
		return err
	}

	log.Info().Msgf("checkpoint done ver=%d dur=%s set=%s del=%s db_sz=%s rate=%s",
		tree.version,
		time.Since(start).Round(time.Millisecond),
		humanize.Comma(int64(len(args.set))),
		humanize.Comma(int64(len(args.delete))),
		humanize.IBytes(uint64(dbSize)),
		humanize.Comma(int64(float64(len(args.set))/time.Since(start).Seconds())),
	)

	if err := tree.sql.queryReport(10); err != nil {
		return err
	}

	return nil
}

func (tree *Tree) checkpoint() error {
	start := time.Now()
	stats := &saveStats{}

	//log.Info().Msgf("dirty_count=%s", humanize.Comma(tree.dirtyCount(tree.root)))

	setCount, err := tree.deepSave(tree.root, stats)
	if err != nil {
		return err
	}

	//sets := tree.buildCheckpoint(tree.root)
	//if len(sets) != int(setCount) {
	//	return fmt.Errorf("set count mismatch; expected=%d, actual=%d", len(sets), setCount)
	//}

	//for _, nk := range tree.orphans {
	//	if err := tree.kv.Delete(nk); err != nil {
	//		return err
	//	}
	//}

	log.Info().Msgf("checkpoint; version=%s, set=%s, del=%s, ws_size=%s, ws_bz=%s, save_bz=%s, db_bz=%s, dur=%s",
		humanize.Comma(tree.version),
		humanize.Comma(setCount),
		humanize.Comma(int64(len(tree.orphans))),
		humanize.Comma(tree.workingSize),
		humanize.IBytes(tree.workingBytes),
		humanize.IBytes(stats.nodeBz),
		humanize.IBytes(stats.dbBz),
		time.Since(start).Round(time.Millisecond),
	)

	return nil
}

type saveStats struct {
	nodeBz uint64
	dbBz   uint64
	count  int64
}

func (tree *Tree) deepHash(sequence *uint32, node *Node) (isLeaf bool, isDirty bool) {

	isLeaf = node.isLeaf()

	// if the node version is equal to the tree version, then this node was updated in this tree version (batch)
	// and must be written to storage.
	if node.nodeKey.Version() == tree.version {
		isDirty = true
		if isLeaf {
			tree.leaves = append(tree.leaves, node)
		} else {
			tree.branches = append(tree.branches, node)
		}
	}

	// tree nodes with a hash are already persisted.
	// leaf nodes with a hash may or may not be persisted.
	// either way we end recursion here.
	if node.hash != nil {
		return isLeaf, isDirty
	}

	// When reading leaves, this will initiate a read from storage for the sole purpose of producing a hash.
	// Recall that a terminal tree node may have only updated one leaf this version.
	// We can explore storing right/left hash in terminal tree nodes to avoid this, or changing the storage
	// format to iavl v0 where left/right hash are stored in the node.
	leftIsLeaf, leftisDirty := tree.deepHash(sequence, node.left(tree))
	rightIsLeaf, rightIsDirty := tree.deepHash(sequence, node.right(tree))

	node._hash(tree.version)

	// will be returned to the pool in BatchSet if not below
	if leftIsLeaf {
		if !leftisDirty {
			tree.pool.Put(node.leftNode)
		}
		node.leftNode = nil
	}
	if rightIsLeaf {
		if !rightIsDirty {
			tree.pool.Put(node.rightNode)
		}
		node.rightNode = nil
	}

	return false, isDirty
}

func (tree *Tree) dirtyCount(node *Node) int64 {
	var n int64
	if node.dirty {
		n = 1
	}

	if !node.isLeaf() {
		return n + tree.dirtyCount(node.left(tree)) + tree.dirtyCount(node.right(tree))
	} else {
		return n
	}
}

func (tree *Tree) buildCheckpoint(node *Node, args *checkpointArgs) {
	if node == nil || node.nodeKey.Version() <= tree.lastCheckpoint {
		return
	}

	tree.cache.Set(node)
	node.dirty = false

	n := tree.pool.Get()
	n.subtreeHeight = node.subtreeHeight
	n.size = node.size
	n.key = node.key
	n.hash = node.hash
	n.nodeKey = node.nodeKey
	n.leftNodeKey = node.leftNodeKey
	n.rightNodeKey = node.rightNodeKey

	if node.isLeaf() {
		args.set = append(args.set, n)
	} else {
		args.set = append(args.set, n)
		tree.buildCheckpoint(node.leftNode, args)
		tree.buildCheckpoint(node.rightNode, args)

		node.leftNode = nil
		node.rightNode = nil
	}
}

func (tree *Tree) deepSave(node *Node, stats *saveStats) (count int64, err error) {
	if node.nodeKey.Version() <= tree.lastCheckpoint {
		return 0, nil
	}

	if n, err := tree.kv.Set(node); err != nil {
		return count, err
	} else {
		stats.dbBz += uint64(n)
	}
	stats.nodeBz += node.sizeBytes()

	node.dirty = false
	tree.cache.Set(node)

	if !node.isLeaf() && node.leftNode != nil {
		leftCount, err := tree.deepSave(node.leftNode, stats)
		if err != nil {
			return count, err
		}
		rightCount, err := tree.deepSave(node.rightNode, stats)
		if err != nil {
			return count, err
		}

		// clear children to prevent memory leaks here. after each checkpoint the tree is purged.
		node.leftNode = nil
		node.rightNode = nil

		return leftCount + rightCount + 1, nil
	} else {
		return 1, nil
	}
}

// Set sets a key in the working tree. Nil values are invalid. The given
// key/value byte slices must not be modified after this call, since they point
// to slices stored within IAVL. It returns true when an existing value was
// updated, while false means it was a new key.
func (tree *Tree) Set(key, value []byte) (updated bool, err error) {

	updated, err = tree.set(key, value)
	if err != nil {
		return false, err
	}
	if updated {
		tree.metrics.TreeUpdate++
	} else {
		tree.metrics.TreeNewNode++
	}

	tree.emitDotGraph(tree.root)

	return updated, nil
}

func (tree *Tree) set(key []byte, value []byte) (updated bool, err error) {
	if value == nil {
		return updated, fmt.Errorf("attempt to store nil value at key '%s'", key)
	}

	if tree.root == nil {
		tree.root = tree.NewNode(key, value, tree.version)
		return updated, nil
	}

	tree.root, updated, err = tree.recursiveSet(tree.root, key, value)
	return updated, err
}

func (tree *Tree) recursiveSet(node *Node, key []byte, value []byte) (
	newSelf *Node, updated bool, err error,
) {
	if node == nil {
		panic("node is nil")
	}
	if node.isLeaf() {
		switch bytes.Compare(key, node.key) {
		case -1: // setKey < leafKey
			tree.metrics.PoolGet += 2
			parent := tree.pool.Get()
			parent.nodeKey = tree.nextNodeKey()
			parent.sortKey = MinRightToken(key, node.key)
			parent.key = node.key
			parent.subtreeHeight = 1
			parent.size = 2
			parent.dirty = true
			parent.setLeft(tree.NewNode(key, value, tree.version))
			parent.setRight(node)

			tree.workingBytes += parent.sizeBytes()
			tree.workingSize++
			return parent, false, nil
		case 1: // setKey > leafKey
			tree.metrics.PoolGet += 2
			parent := tree.pool.Get()
			parent.nodeKey = tree.nextNodeKey()
			parent.sortKey = MinRightToken(key, node.key)
			parent.key = key
			parent.subtreeHeight = 1
			parent.size = 2
			parent.dirty = true
			parent.setLeft(node)
			parent.setRight(tree.NewNode(key, value, tree.version))

			tree.workingBytes += parent.sizeBytes()
			tree.workingSize++
			return parent, false, nil
		default:
			tree.mutateNode(node)
			node.value = value
			node._hash(tree.version + 1)
			node.value = nil
			return node, true, nil
		}

	} else {
		tree.addOrphan(node)
		tree.mutateNode(node)

		var child *Node
		if bytes.Compare(key, node.key) < 0 {
			child, updated, err = tree.recursiveSet(node.left(tree), key, value)
			if err != nil {
				return nil, updated, err
			}
			node.setLeft(child)
		} else {
			child, updated, err = tree.recursiveSet(node.right(tree), key, value)
			if err != nil {
				return nil, updated, err
			}
			node.setRight(child)
		}

		if updated {
			return node, updated, nil
		}

		//if bytes.Equal(node.leftNode.key, key) {
		//	node.sortKey = MinRightToken(node.leftNode.key, node.rightNode.key)
		//}

		// case:
		// at insert time node.sortKey = f, and node.key = fe777f
		// and key = fe611...
		// this behavior produces a new sortKey = fe
		//
		// want:
		// sortKey = fe7
		if bytes.HasPrefix(key, node.sortKey) {
			if bytes.Compare(key, node.key) < 0 {
				node.sortKey = MinRightToken(node.key, key)
			} else {
				node.sortKey = MinLeftToken(node.key, key)
			}
			//node.sortKey = node.key[:len(node.sortKey)+1]
			//node.sortKey = MinRightToken(node.key, key)
			//node.sortKey = MinLeftToken(node.key, key)
		}

		err = node.calcHeightAndSize(tree)
		if err != nil {
			return nil, false, err
		}
		newNode, err := tree.balance(node)
		if err != nil {
			return nil, false, err
		}
		return newNode, updated, err
	}
}

// Remove removes a key from the working tree. The given key byte slice should not be modified
// after this call, since it may point to data stored inside IAVL.
func (tree *Tree) Remove(key []byte) ([]byte, bool, error) {
	if tree.root == nil {
		return nil, false, nil
	}
	newRoot, _, value, removed, err := tree.recursiveRemove(tree.root, key)
	if err != nil {
		return nil, false, err
	}
	if !removed {
		return nil, false, nil
	}

	tree.metrics.TreeDelete++

	tree.root = newRoot
	tree.emitDotGraph(tree.root)
	return value, true, nil
}

// removes the node corresponding to the passed key and balances the tree.
// It returns:
// - the hash of the new node (or nil if the node is the one removed)
// - the node that replaces the orig. node after remove
// - new leftmost leaf key for tree after successfully removing 'key' if changed.
// - the removed value
func (tree *Tree) recursiveRemove(node *Node, key []byte) (newSelf *Node, newKey []byte, newValue []byte, removed bool, err error) {
	if node.isLeaf() {
		if bytes.Equal(key, node.key) {
			tree.returnNode(node)
			return nil, nil, node.value, true, nil
		}
		return node, nil, nil, false, nil
	}

	if err != nil {
		return nil, nil, nil, false, err
	}

	// node.key < key; we go to the left to find the key:
	if bytes.Compare(key, node.key) < 0 {
		newLeftNode, newKey, value, removed, err := tree.recursiveRemove(node.left(tree), key)
		if err != nil {
			return nil, nil, nil, false, err
		}

		if !removed {
			return node, nil, value, removed, nil
		}

		// left node held value, was removed
		// collapse `node.rightNode` into `node`
		if newLeftNode == nil {
			right := node.right(tree)
			k := node.key
			tree.returnNode(node)
			return right, k, value, removed, nil
		}

		tree.addOrphan(node)
		tree.mutateNode(node)

		node.setLeft(newLeftNode)
		err = node.calcHeightAndSize(tree)
		if err != nil {
			return nil, nil, nil, false, err
		}
		node, err = tree.balance(node)
		if err != nil {
			return nil, nil, nil, false, err
		}

		return node, newKey, value, removed, nil
	}
	// node.key >= key; either found or look to the right:
	newRightNode, newKey, value, removed, err := tree.recursiveRemove(node.right(tree), key)
	if err != nil {
		return nil, nil, nil, false, err
	}

	if !removed {
		return node, nil, value, removed, nil
	}

	// right node held value, was removed
	// collapse `node.leftNode` into `node`
	if newRightNode == nil {
		left := node.left(tree)
		tree.returnNode(node)
		return left, nil, value, removed, nil
	}

	tree.addOrphan(node)
	tree.mutateNode(node)

	node.setRight(newRightNode)
	if newKey != nil {
		node.key = newKey
	}
	err = node.calcHeightAndSize(tree)
	if err != nil {
		return nil, nil, nil, false, err
	}

	node, err = tree.balance(node)
	if err != nil {
		return nil, nil, nil, false, err
	}

	return node, nil, value, removed, nil
}

func (tree *Tree) Size() int64 {
	return tree.root.size
}

func (tree *Tree) Height() int8 {
	return tree.root.subtreeHeight
}

func (tree *Tree) nextNodeKey() NodeKey {
	tree.sequence++
	nk := NewNodeKey(tree.version+1, tree.sequence)
	return nk
}

func (tree *Tree) mutateNode(node *Node) {
	// node has already been mutated in working set
	if node.hash == nil {
		return
	}
	node.hash = nil
	node.nodeKey = tree.nextNodeKey()

	if node.dirty {
		return
	}

	node.dirty = true
	tree.workingBytes += node.sizeBytes()
	tree.workingSize++
}

func (tree *Tree) addOrphan(node *Node) {
	if node.hash == nil {
		return
	}
	tree.orphans = append(tree.orphans, &nodeDiff{
		new:        node,
		prevHeight: node.subtreeHeight,
		prevKey:    node.key,
	})
}

// NewNode returns a new node from a key, value and version.
func (tree *Tree) NewNode(key []byte, value []byte, version int64) *Node {
	//node := &Node{
	//	key:           key,
	//	value:         value,
	//	subtreeHeight: 0,
	//	size:          1,
	//}
	node := tree.pool.Get()

	node.nodeKey = tree.nextNodeKey()

	node.key = key
	node.sortKey = key
	node.value = value
	node.subtreeHeight = 0
	node.size = 1

	node._hash(version + 1)
	node.value = nil
	node.dirty = true
	tree.workingBytes += node.sizeBytes()
	tree.workingSize++
	return node
}

func (tree *Tree) returnNode(node *Node) {
	if node.dirty {
		tree.workingBytes -= node.sizeBytes()
		tree.workingSize--
	}
	tree.orphans = append(tree.orphans, &nodeDiff{
		deleted:    true,
		prevKey:    node.key,
		prevHeight: node.subtreeHeight,
	})
	tree.pool.Put(node)
}

func (tree *Tree) emitDotGraph(root *Node) *dot.Graph {
	if !tree.emitDotGraphs {
		return nil
	}
	tree.lastDotGraph = writeDotGraph(root, tree.lastDotGraph)
	tree.dotGraphs = append(tree.dotGraphs, tree.lastDotGraph)
	return tree.lastDotGraph
}
