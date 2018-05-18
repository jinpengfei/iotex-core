// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package trie

import (
	"container/list"

	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/common"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/logger"
)

var (
	// ErrInvalidTrie: something wrong causing invalid operation
	ErrInvalidTrie = errors.New("invalid trie operation")
)

var (
	// emptyRoot is the root hash of an empty trie
	emptyRoot = common.Hash32B{0xe, 0x57, 0x51, 0xc0, 0x26, 0xe5, 0x43, 0xb2, 0xe8, 0xab, 0x2e, 0xb0, 0x60, 0x99,
		0xda, 0xa1, 0xd1, 0xe5, 0xdf, 0x47, 0x77, 0x8f, 0x77, 0x87, 0xfa, 0xab, 0x45, 0xcd, 0xf1, 0x2f, 0xe3, 0xa8}
)

type (
	// Trie is the interface of Merkle Patricia Trie
	Trie interface {
		Insert(key, value []byte) error // insert a new entry
		Get(key []byte) ([]byte, error) // retrieve an existing entry
		Update(key, value []byte) error // update an existing entry
		Delete(key []byte) error        // delete an entry
		Close() error                   // close the trie DB
		RootHash() common.Hash32B       // returns trie's root hash
	}

	// trie implements the Trie interface
	trie struct {
		dao       db.KVStore
		root      patricia
		curr      patricia   // current patricia node when updating nodes on path ascending to root
		toRoot    *list.List // stores the path from root to diverging node
		addNode   *list.List // stored newly added nodes on insert() operation
		bucket    string     // bucket name to store the nodes
		clpsK     []byte     // path if the node can collapse after deleting an entry
		clpsV     []byte     // value if the node can collapse after deleting an entry
		clpsType  byte       // collapse into which node: 1-extension, 0-leaf
		numEntry  uint64     // number of entries added to the trie
		numBranch uint64
		numExt    uint64
		numLeaf   uint64
	}
)

// NewTrie creates a trie with DB filename
func NewTrie(path string) (Trie, error) {
	dao := db.NewBoltDB(path, nil)
	if dao == nil {
		return nil, errors.New("Cannot create boltDB file")
	}
	t := trie{dao: dao, root: &branch{}, toRoot: list.New(), addNode: list.New(), bucket: "trie", numEntry: 1, numBranch: 1}
	if err := dao.Start(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Close close the DB
func (t *trie) Close() error {
	return t.dao.Stop()
}

// Insert a new entry
func (t *trie) Insert(key, value []byte) error {
	div, size, err := t.query(key)
	if err == nil {
		return errors.Wrapf(ErrInvalidTrie, "key = %x already exist", key)
	}
	// insert at the diverging patricia node
	nb, ne, nl := div.increase(key[size:])
	if err := div.insert(key[size:], value, t.addNode); err != nil {
		return errors.Wrapf(err, "failed to insert key = %x", key)
	}
	// update newly added patricia node into DB
	var hashChild common.Hash32B
	for t.addNode.Len() > 0 {
		n := t.addNode.Back()
		ptr, ok := n.Value.(patricia)
		if !ok {
			return errors.Wrapf(ErrInvalidPatricia, "cannot decode node = %v", n.Value)
		}
		hashChild = ptr.hash()
		// hash of new node should NOT exist in DB
		if err := t.putPatriciaNew(ptr); err != nil {
			return err
		}
		t.addNode.Remove(n)
	}
	t.numBranch += uint64(nb)
	t.numExt += uint64(ne)
	t.numLeaf += uint64(nl)
	t.numEntry++
	// if the diverging node is leaf, it will be replaced and no need to update
	n := t.toRoot.Back()
	if _, ok := n.Value.(patricia).(*leaf); ok {
		logger.Warn().Msg("discard leaf")
		t.toRoot.Remove(n)
	}
	// update upstream nodes on path ascending to root
	return t.updateInsert(hashChild[:])
}

// Get an existing entry
func (t *trie) Get(key []byte) ([]byte, error) {
	ptr, size, err := t.query(key)
	t.clear()
	if size != len(key) {
		return nil, errors.Wrapf(ErrInvalidTrie, "key = %x not exist", key)
	}
	if err != nil {
		return nil, err
	}
	// retrieve the value from terminal patricia node
	size = len(key)
	return t.getValue(ptr, key[size-1])
}

// Update an existing entry
func (t *trie) Update(key, value []byte) error {
	var ptr patricia
	var size int
	var err error
	ptr, size, err = t.query(key)
	if size != len(key) {
		return errors.Wrapf(ErrInvalidTrie, "key = %x not exist", key)
	}
	if err != nil {
		return err
	}
	var index byte
	t.clpsK, t.clpsV = nil, nil
	if _, ok := ptr.(*branch); ok {
		// for branch, the entry to delete is the leaf matching last byte of path
		size = len(key)
		index = key[size-1]
		if t.curr, err = t.getPatricia(ptr.(*branch).Path[index]); err != nil {
			return err
		}
	} else {
		t.curr, index = t.popToRoot()
	}
	// delete the entry and update if it can collapse
	if _, err = t.delete(t.curr, index); err != nil {
		return err
	}
	// update with new value
	t.curr.set(value, index)
	if err := t.putPatricia(t.curr); err != nil {
		return err
	}
	// update upstream nodes on path ascending to root
	hashChild := t.curr.hash()
	return t.updateInsert(hashChild[:])
}

// Delete an entry
func (t *trie) Delete(key []byte) error {
	var ptr patricia
	var size int
	var err error
	ptr, size, err = t.query(key)
	if size != len(key) {
		return errors.Wrapf(ErrInvalidTrie, "key = %x not exist", key)
	}
	if err != nil {
		return err
	}
	var index byte
	var childClps bool
	t.clpsK, t.clpsV = nil, nil
	if _, ok := ptr.(*branch); ok {
		// for branch, the entry to delete is the leaf matching last byte of path
		size = len(key)
		index = key[size-1]
		if t.curr, err = t.getPatricia(ptr.(*branch).Path[index]); err != nil {
			return err
		}
	} else {
		t.curr, index = t.popToRoot()
	}
	// delete the entry and update if it can collapse
	if childClps, err = t.delete(t.curr, index); err != nil {
		return err
	}
	if t.numEntry == 1 {
		return errors.Wrapf(ErrInvalidTrie, "trie has more entries than ever added")
	}
	t.numEntry--
	if t.numEntry == 2 {
		// only 1 entry left (the other being the root), collapse into leaf
		t.clpsType = 0
	}
	// update upstream nodes on path ascending to root
	return t.updateDelete(childClps)
}

// RootHash returns the root hash of merkle patricia trie
func (t *trie) RootHash() common.Hash32B {
	return t.root.hash()
}

//======================================
// private functions
//======================================
// query returns the diverging patricia node, and length of matching path in bytes
func (t *trie) query(key []byte) (patricia, int, error) {
	ptr := t.root
	size := 0
	for len(key) > 0 {
		// keep descending the trie
		hashn, match, err := ptr.descend(key)
		logger.Debug().Hex("key", hashn).Msg("access")
		if _, b := ptr.(*branch); b {
			// for branch node, need to save first byte of path to traceback to branch[key[0]] later
			t.toRoot.PushBack(key[0])
		}
		t.toRoot.PushBack(ptr)
		// path diverges, return the diverging node
		if err != nil {
			// patricia.insert() will be called later to insert <key, value> pair into trie
			return ptr, size, err
		}
		// path matching entire key, return ptr that holds the value
		if match == len(key) {
			return ptr, size + match, nil
		}
		if ptr, err = t.getPatricia(hashn); err != nil {
			return nil, 0, err
		}
		size += match
		key = key[match:]
	}
	return nil, size, nil
}

// delete removes the entry stored in patricia node, and returns if the node can collapse
func (t *trie) delete(ptr patricia, index byte) (bool, error) {
	var childClps bool
	// delete the node from DB
	if err := t.delPatricia(ptr); err != nil {
		return childClps, err
	}
	// by default assuming collapse to leaf node
	t.clpsType = 0
	switch ptr.(type) {
	case *branch:
		// check if the branch can collapse, and if yes get the leaf node value
		if t.clpsK, t.clpsV, childClps = ptr.collapse(t.clpsK, t.clpsV, index, true); childClps {
			l, err := t.getPatricia(t.clpsV)
			if err != nil {
				return childClps, err
			}
			// the original branch collapse to its single remaining leaf
			var k []byte
			if k, t.clpsV, err = l.blob(); err != nil {
				return childClps, err
			}
			// remaining leaf path != nil means it is extension node
			if k != nil {
				t.clpsType = 1
			}
			t.clpsK = append(t.clpsK, k...)
			ptr.(*branch).print()
		}
	case *leaf:
		if ptr.(*leaf).Ext == 1 {
			return childClps, errors.Wrap(ErrInvalidPatricia, "extension cannot be terminal node")
		}
		// deleting a leaf, upstream node must be extension so collapse into extension
		childClps, t.clpsType = true, 1
	}
	return childClps, nil
}

// updateInsert rewinds the path back to root and updates nodes along the way
func (t *trie) updateInsert(hashChild []byte) error {
	for t.toRoot.Len() > 0 {
		var index byte
		t.curr, index = t.popToRoot()
		if t.curr == nil {
			return errors.Wrap(ErrInvalidPatricia, "patricia pushed on stack is not valid")
		}
		// delete the current node
		hashCurr := t.curr.hash()
		logger.Info().Hex("curr key", hashCurr[:8]).Msg("10-4")
		logger.Info().Hex("child key", hashChild[:8]).Msg("10-4")
		if err := t.delPatricia(t.curr); err != nil {
			return err
		}
		// update the patricia node
		if err := t.curr.ascend(hashChild[:], index); err != nil {
			return err
		}
		hashCurr = t.curr.hash()
		hashChild = hashCurr[:]
		// when adding an entry, hash of nodes along the path changes and is expected NOT to exist in DB
		if err := t.putPatriciaNew(t.curr); err != nil {
			return err
		}
	}
	return nil
}

// updateDelete rewinds the path back to root and updates nodes along the way
func (t *trie) updateDelete(currClps bool) error {
	contClps := false
	for t.toRoot.Len() > 0 {
		logger.Info().Int("stack size", t.toRoot.Len()).Msg("clps")
		next, index := t.popToRoot()
		if next == nil {
			return errors.Wrap(ErrInvalidPatricia, "patricia pushed on stack is not valid")
		}
		if err := t.delPatricia(next); err != nil {
			return err
		}
		// we attempt to collapse in 2 cases:
		// 1. the current node is not root
		// 2. the current node is root, but <k, v> is nil meaning no more entries exist on the incoming path
		isRoot := t.toRoot.Len() == 0
		noEntry := t.clpsK == nil && t.clpsV == nil
		var nextClps bool
		t.clpsK, t.clpsV, nextClps = next.collapse(t.clpsK, t.clpsV, index, currClps && (!isRoot || noEntry))
		logger.Info().Bool("curr", currClps).Msg("clps")
		logger.Info().Bool("next", nextClps).Msg("clps")
		if nextClps {
			// current node can also collapse, concatenate the path and keep going
			contClps = true
			if !isRoot {
				currClps = nextClps
				t.curr = next
				continue
			}
		}
		logger.Info().Bool("cont", contClps).Msg("clps")
		if contClps {
			// only 1 entry (which is the root) left, the trie fallback to an empty trie
			if isRoot && t.numEntry == 1 {
				t.root = nil
				t.root = &branch{}
				logger.Warn().Msg("all entries deleted, trie fallback to empty")
				return nil
			}
			// otherwise collapse into a leaf node
			if t.clpsV != nil{
				t.curr = &leaf{t.clpsType, t.clpsK, t.clpsV}
				logger.Info().Hex("k", t.clpsK).Hex("v", t.clpsV).Msg("clps")
				// after collapsing, the trie might rollback to an earlier state in the history (before adding the deleted entry)
				// so the node we try to put may already exist in DB
				if err := t.putPatricia(t.curr); err != nil {
					return err
				}
			}
		}
		contClps = false
		// update current with new child
		hash := t.curr.hash()
		next.ascend(hash[:], index)
		// for the same reason above, the trie might rollback to an earlier state in the history
		// so the node we try to put may already exist in DB
		if err := t.putPatricia(next); err != nil {
			return err
		}
		currClps = nextClps
		t.curr = next
	}
	return nil
}

//======================================
// helper functions to operate patricia
//======================================
// getPatricia retrieves the patricia node from DB according to key
func (t *trie) getPatricia(key []byte) (patricia, error) {
	node, err := t.dao.Get(t.bucket, key)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get key %x", key[:8])
	}
	var ptr patricia
	// first byte of serialized data is type
	switch node[0] {
	case 2:
		ptr = &branch{}
	case 1:
		ptr = &leaf{}
	case 0:
		ptr = &leaf{}
	default:
		return nil, errors.Wrapf(ErrInvalidPatricia, "invalid node type = %v", node[0])
	}
	if err := ptr.deserialize(node); err != nil {
		return nil, err
	}
	return ptr, nil
}

// putPatricia stores the patricia node into DB
// the node may already exist in DB
func (t *trie) putPatricia(ptr patricia) error {
	value, err := ptr.serialize()
	if err != nil {
		return errors.Wrapf(err, "failed to encode node")
	}
	key := ptr.hash()
	if err := t.dao.Put(t.bucket, key[:], value); err != nil {
		return errors.Wrapf(err, "failed to put key = %x", key[:8])
	}
	logger.Debug().Hex("key", key[:8]).Msg("put")
	return nil
}

// putPatriciaNew stores a new patricia node into DB
// it is expected the node does not exist yet, will return error if already exist
func (t *trie) putPatriciaNew(ptr patricia) error {
	value, err := ptr.serialize()
	if err != nil {
		return err
	}
	key := ptr.hash()
	if err := t.dao.PutIfNotExists(t.bucket, key[:], value); err != nil {
		return errors.Wrapf(err, "failed to put non-existing key = %x", key[:8])
	}
	logger.Debug().Hex("key", key[:8]).Msg("putnew")
	return nil
}

// delPatricia deletes the patricia node from DB
func (t *trie) delPatricia(ptr patricia) error {
	key := ptr.hash()
	if err := t.dao.Delete(t.bucket, key[:]); err != nil {
		return errors.Wrapf(err, "failed to delete key = %x", key[:8])
	}
	logger.Debug().Hex("key", key[:8]).Msg("del")
	return nil
}

// getValue returns the actual value stored in patricia node
func (t *trie) getValue(ptr patricia, index byte) ([]byte, error) {
	br, isBranch := ptr.(*branch)
	var err error
	if isBranch {
		if ptr, err = t.getPatricia(br.Path[index]); err != nil {
			return nil, err
		}
	}
	_, v, e := ptr.blob()
	return v, e
}

// clear the stack
func (t *trie) clear() {
	for t.toRoot.Len() > 0 {
		n := t.toRoot.Back()
		t.toRoot.Remove(n)
	}
}

// pop the stack
func (t *trie) popToRoot() (patricia, byte) {
	if t.toRoot.Len() > 0 {
		n := t.toRoot.Back()
		ptr, _ := n.Value.(patricia)
		t.toRoot.Remove(n)
		var index byte
		_, isBranch := ptr.(*branch)
		if isBranch {
			// for branch node, the index is pushed onto stack in query()
			n := t.toRoot.Back()
			index, _ = n.Value.(byte)
			t.toRoot.Remove(n)
		}
		return ptr, index
	}
	return nil, 0
}

func (t *trie) stat() {
	logger.Info().Uint64("B", t.numBranch).Uint64("E", t.numExt).Uint64("L", t.numLeaf).Uint64("Item", t.numEntry).Msg("Counting")
}