package app

import "github.com/tendermint/tmlibs/merkle"

// State represents the app states, separating the commited state (for queries)
// from the working state (for CheckTx and AppendTx)
type State struct {
	committed  merkle.Tree
	deliverTx  merkle.Tree
	checkTx    merkle.Tree
	persistent bool
}

func NewState(tree merkle.Tree, persistent bool) State {
	return State{
		committed:  tree,
		deliverTx:  tree.Copy(),
		checkTx:    tree.Copy(),
		persistent: persistent,
	}
}

func (s State) Committed() merkle.Tree {
	return s.committed
}

func (s State) Append() merkle.Tree {
	return s.deliverTx
}

func (s State) Check() merkle.Tree {
	return s.checkTx
}

// PreCommit stores the current Append() state as committed
// starts new Append/Check state, and
// returns the hash for the commit
func (s *State) Hash() []byte {
	var hash []byte
	if s.persistent {
		// Don't save it right now, just calc the hash
		hash = s.deliverTx.Hash()
	} else {
		hash = s.deliverTx.Hash()
	}
	return hash
}

func (s *State) Commit() []byte {
	var hash []byte
	if s.persistent {
		hash = s.committed.Save()
	} else {
		hash = s.committed.Hash()
	}
	s.committed = s.deliverTx
	s.deliverTx = s.committed.Copy()
	s.checkTx = s.committed.Copy()
	return hash
}
