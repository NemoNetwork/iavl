package iavl

import (
	"sync"
)

type nodePool struct {
	syncPool *sync.Pool
	free     chan int
	nodes    []Node
	poolSize uint64
}

const initialNodePoolSize = 1_000_000

func newNodePool() *nodePool {
	np := &nodePool{
		syncPool: &sync.Pool{
			New: func() interface{} {
				return &Node{}
			},
		},
		free: make(chan int, 100_000_000),
	}
	np.grow(initialNodePoolSize)
	return np
}

func (np *nodePool) grow(amount int) {
	startSize := len(np.nodes)
	log.Warn().Msgf("growing node pool amount=%d; size=%d", amount, startSize+amount)
	for i := startSize; i < startSize+amount; i++ {
		np.free <- i
		np.nodes = append(np.nodes, Node{poolId: i})
		np.poolSize += nodeSize
	}
}

func (np *nodePool) Get() *Node {
	if len(np.free) == 0 {
		np.grow(len(np.nodes))
	}
	poolId := <-np.free
	node := &np.nodes[poolId]
	if node.hash != nil {
		panic("invariant violated: node hash should be nil when fetched from pool")
	}
	return node
}

func (np *nodePool) Put(node *Node) {
	np.resetNode(node)
	np.free <- node.poolId
}

func (np *nodePool) resetNode(node *Node) {
	node.leftNodeKey = emptyNodeKey
	node.rightNodeKey = emptyNodeKey
	node.rightNode = nil
	node.leftNode = nil
	node.nodeKey = emptyNodeKey
	node.hash = nil
	node.key = nil
	node.value = nil
	node.subtreeHeight = 0
	node.size = 0
	node.dirty = false
}
