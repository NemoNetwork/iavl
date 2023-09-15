// TODO move to package iavl_test
// this means an audit of exported fields and types.
package iavl

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/cosmos/iavl/v2/leveldb"
	"github.com/cosmos/iavl/v2/metrics"
	"github.com/cosmos/iavl/v2/testutil"
	"github.com/dustin/go-humanize"
	"github.com/stretchr/testify/require"
)

func PrintMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// For info on each, see: https://golang.org/pkg/runtime/#MemStats
	fmt.Printf("*** Memory Stats; Alloc = %s", humanize.Bytes(m.Alloc))
	fmt.Printf("\tTotalAlloc = %s", humanize.Bytes(m.TotalAlloc))
	fmt.Printf("\tSys = %s", humanize.Bytes(m.Sys))
	fmt.Printf("\tNumGC = %v\n", humanize.Comma(int64(m.NumGC)))
}

func testTreeBuild(t *testing.T, tree *Tree, opts testutil.TreeBuildOptions) (cnt int64) {
	var (
		hash    []byte
		version int64
		since   = time.Now()
		err     error
	)
	cnt = 1

	// log file
	//itr, err := compact.NewChangesetIterator("/Users/mattk/src/scratch/osmosis-hist/ordered/bank", "bank")
	//require.NoError(t, err)
	//opts.Until = math.MaxInt64

	// generator
	itr := opts.Iterator

	fmt.Printf("Initial memory usage from generators:\n")
	PrintMemUsage()

	itrStart := time.Now()
	for ; itr.Valid(); err = itr.Next() {
		require.NoError(t, err)
		changeset := itr.Nodes()
		for ; changeset.Valid(); err = changeset.Next() {
			require.NoError(t, err)
			node := changeset.GetNode()
			var keyBz bytes.Buffer
			keyBz.Write([]byte(node.StoreKey))
			keyBz.Write(node.Key)
			key := keyBz.Bytes()

			if !node.Delete {
				_, err = tree.Set(key, node.Value)
				require.NoError(t, err)
			} else {
				_, _, err := tree.Remove(key)
				require.NoError(t, err)
			}

			if cnt%100_000 == 0 {
				fmt.Printf("processed %s leaves in %s; leaves/s last=%s μ=%s; version=%d\n",
					humanize.Comma(int64(cnt)),
					time.Since(since),
					humanize.Comma(int64(100_000/time.Since(since).Seconds())),
					humanize.Comma(int64(float64(cnt)/time.Since(itrStart).Seconds())),
					version)
				since = time.Now()
				PrintMemUsage()
			}
			cnt++
		}
		hash, version, err = tree.SaveVersion()
		require.NoError(t, err)
		if version == opts.Until {
			break
		}
	}
	fmt.Printf("final version: %d, hash: %x\n", version, hash)
	fmt.Printf("height: %d, size: %d\n", tree.Height(), tree.Size())
	fmt.Printf("mean leaves/ms %s\n", humanize.Comma(cnt/time.Since(itrStart).Milliseconds()))
	if opts.Report != nil {
		opts.Report()
	}
	require.Equal(t, version, opts.Until)
	return cnt
}

func TestTree_Build(t *testing.T) {
	//just a little bigger than the size of the initial changeset. evictions will occur slowly.
	//poolSize := 210_050
	// no evictions
	poolSize := 10_000
	// overflow on initial changeset and frequently after; worst performance
	//poolSize := 100_000

	var err error
	//db := newMapDB()

	tmpDir := t.TempDir()
	t.Logf("levelDb tmpDir: %s\n", tmpDir)
	levelDb, err := leveldb.New("iavl_test", tmpDir)
	require.NoError(t, err)
	db := &kvDB{db: levelDb}

	tree := &Tree{
		pool:               newNodePool(db, poolSize),
		metrics:            &metrics.TreeMetrics{},
		db:                 db,
		checkpointInterval: 100_000,
	}
	tree.pool.metrics = tree.metrics
	tree.pool.maxWorkingSize = 200 * 1024 * 1024

	//opts := testutil.BankLockup25_000()
	//opts := testutil.NewTreeBuildOptions()
	opts := testutil.BigStartOptions()
	opts.Report = func() {
		tree.metrics.Report()
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		checkpointErr := tree.pool.CheckpointRunner(ctx)
		require.NoError(t, checkpointErr)
	}()

	testStart := time.Now()
	leaves := testTreeBuild(t, tree, opts)

	err = tree.Checkpoint()
	require.NoError(t, err)
	// wait
	tree.pool.checkpointCh <- &checkpointArgs{version: -1}
	treeDuration := time.Since(testStart)

	// don't evict root on iteration, it interacts with the node pool
	tree.root.dirty = true
	count := pooledTreeCount(tree, *tree.root)
	height := pooledTreeHeight(tree, *tree.root)

	workingSetCount := 0 // offset the dirty root above.
	for _, n := range tree.pool.nodes {
		if n.dirty {
			workingSetCount++
		}
	}

	fmt.Printf("mean leaves/s: %s\n", humanize.Comma(int64(float64(leaves)/treeDuration.Seconds())))
	fmt.Printf("workingSetCount: %d\n", workingSetCount)
	fmt.Printf("treeCount: %d\n", count)
	fmt.Printf("treeHeight: %d\n", height)

	// TODO
	// equivalence between dbs

	//fmt.Printf("db stats:\n sets: %s, deletes: %s\n",
	//	humanize.Comma(int64(db.setCount)),
	//	humanize.Comma(int64(db.deleteCount)))

	require.Equal(t, height, tree.root.SubtreeHeight+1)
	//require.Equal(t, count, len(db.nodes))

	require.Equal(t, tree.pool.dirtyCount, workingSetCount)

	treeAndDbEqual(t, tree, *tree.root)

	require.Equal(t, opts.UntilHash, fmt.Sprintf("%x", tree.root.hash))
}

func treeCount(node *Node) int {
	if node == nil {
		return 0
	}
	return 1 + treeCount(node.leftNode) + treeCount(node.rightNode)
}

func pooledTreeCount(tree *Tree, node Node) int {
	if node.isLeaf() {
		return 1
	}
	left := *node.left(tree)
	right := *node.right(tree)
	return 1 + pooledTreeCount(tree, left) + pooledTreeCount(tree, right)
}

func pooledTreeHeight(tree *Tree, node Node) int8 {
	if node.isLeaf() {
		return 1
	}
	left := *node.left(tree)
	right := *node.right(tree)
	return 1 + maxInt8(pooledTreeHeight(tree, left), pooledTreeHeight(tree, right))
}

func treeAndDbEqual(t *testing.T, tree *Tree, node Node) {
	dbNode, err := tree.db.Get(node.NodeKey)
	if err != nil {
		t.Fatalf("error getting node from db: %s", err)
	}
	require.NoError(t, err)
	require.NotNil(t, dbNode)
	require.Equal(t, dbNode.NodeKey, node.NodeKey)
	require.Equal(t, dbNode.Key, node.Key)
	require.Equal(t, dbNode.hash, node.hash)
	require.Equal(t, dbNode.Size, node.Size)
	require.Equal(t, dbNode.SubtreeHeight, node.SubtreeHeight)
	if node.isLeaf() {
		return
	}
	require.Equal(t, dbNode.LeftNodeKey, node.LeftNodeKey)
	require.Equal(t, dbNode.RightNodeKey, node.RightNodeKey)

	leftNode := *node.left(tree)
	rightNode := *node.right(tree)
	treeAndDbEqual(t, tree, leftNode)
	treeAndDbEqual(t, tree, rightNode)
}
