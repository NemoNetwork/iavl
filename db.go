package iavl

import (
	"crypto/sha256"
	"fmt"
)

const (
	hashSize = sha256.Size
)

type DB interface {
	Get(key []byte) ([]byte, error)
	Set(key []byte, value []byte) error
	Delete(key []byte) error
}

type nodeDB interface {
	Set(node *Node) error
	Get(nk NodeKey) (*Node, error)
	Delete(nk NodeKey) error
}

type kvDB struct {
	db DB
}

func (kv *kvDB) Set(node *Node) error {
	bz, err := node.Bytes()
	if err != nil {
		return err
	}
	return kv.db.Set(node.NodeKey[:], bz)
}

func (kv *kvDB) Get(nk NodeKey) (*Node, error) {
	bz, err := kv.db.Get(nk[:])
	if err != nil {
		return nil, err
	}
	if bz == nil {
		return nil, fmt.Errorf("node not found: %s", nk.String())
	}
	n, err := MakeNode(nk[:], bz)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (kv *kvDB) Delete(nk NodeKey) error {
	return kv.db.Delete(nk[:])
}

// mapDB approximates a database with a map.
// it used to store nodes in memory so that pool size can be constrained and tested.
type mapDB struct {
	nodes          map[NodeKey]Node
	setCount       int
	deleteCount    int
	lastCheckpoint int64
}

func newMapDB() *mapDB {
	return &mapDB{
		nodes: make(map[NodeKey]Node),
	}
}

func (db *mapDB) Set(node *Node) error {
	n := *node
	n.dirty = false
	n.leftNode = nil
	n.rightNode = nil
	n.frameId = -1
	db.nodes[node.NodeKey] = n
	db.setCount++
	return nil
}

func (db *mapDB) Get(nk NodeKey) (*Node, error) {
	n, ok := db.nodes[nk]
	if !ok {
		return nil, nil
	}
	return &n, nil
}

func (db *mapDB) Delete(nk NodeKey) error {
	delete(db.nodes, nk)
	db.deleteCount++
	return nil
}