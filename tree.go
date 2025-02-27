// This is free and unencumbered software released into the public domain.
//
// Anyone is free to copy, modify, publish, use, compile, sell, or
// distribute this software, either in source code form or as a compiled
// binary, for any purpose, commercial or non-commercial, and by any
// means.
//
// In jurisdictions that recognize copyright laws, the author or authors
// of this software dedicate any and all copyright interest in the
// software to the public domain. We make this dedication for the benefit
// of the public at large and to the detriment of our heirs and
// successors. We intend this dedication to be an overt act of
// relinquishment in perpetuity of all present and future rights to this
// software under copyright law.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
// IN NO EVENT SHALL THE AUTHORS BE LIABLE FOR ANY CLAIM, DAMAGES OR
// OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
// ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
// OTHER DEALINGS IN THE SOFTWARE.
//
// For more information, please refer to <https://unlicense.org>

package verkle

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/protolambda/go-kzg/bls"
)

type NodeFlushFn func(VerkleNode)
type NodeResolverFn func([]byte) ([]byte, error)

// Committer represents an object that is able to create the
// commitment to a polynomial.
type Committer interface {
	CommitToPoly([]bls.Fr, int) *bls.G1Point
}

type VerkleNode interface {
	// Insert or Update value into the tree
	Insert([]byte, []byte, NodeResolverFn) error

	// Insert "à la" Stacktrie. Same thing as insert, except that
	// values are expected to be ordered, and the commitments and
	// hashes for each subtrie are computed online, as soon as it
	// is clear that no more values will be inserted in there.
	InsertOrdered([]byte, []byte, NodeFlushFn) error

	// Delete a leaf with the given key
	Delete([]byte) error

	// Get value at a given key
	Get([]byte, NodeResolverFn) ([]byte, error)

	// ComputeCommitment computes the commitment of the node
	// The results (the curve point and the field element
	// representation of its hash) are cached.
	ComputeCommitment() *bls.Fr

	// GetCommitmentAlongPath follows the path that one key
	// traces through the tree, and collects the various
	// elements needed to build a proof. The order of elements
	// is from the bottom of the tree, up to the root.
	GetCommitmentsAlongPath([]byte) ([]*bls.G1Point, []uint, []*bls.Fr, [][]bls.Fr)

	// Serialize encodes the node to RLP.
	Serialize() ([]byte, error)

	// Copy a node and its children
	Copy() VerkleNode

	// toDot returns a string representing this subtree in DOT language
	toDot(string, string) string
}

const (
	// These types will distinguish internal
	// and leaf nodes when decoding from RLP.
	internalRLPType byte = 1
	leafRLPType     byte = 2
)

var (
	errInsertIntoHash      = errors.New("trying to insert into hashed node")
	errDeleteNonExistent   = errors.New("trying to delete non-existent leaf")
	errReadFromInvalid     = errors.New("trying to read from an invalid child")
	errSerializeHashedNode = errors.New("trying to serialized a hashed node")
)

type (
	// Represents an internal node at any level
	InternalNode struct {
		// List of child nodes of this internal node.
		children []VerkleNode

		// node depth in the tree, in bits
		depth int

		// child count, used for the special case in
		// commitment calculations.
		count uint

		// Cache the field representation of the hash
		// of the current node.
		hash *bls.Fr

		// Cache the commitment value
		commitment *bls.G1Point

		committer Committer
	}

	HashedNode struct {
		hash       *bls.Fr
		commitment *bls.G1Point
	}

	LeafNode struct {
		key    []byte
		values [][]byte

		commitment *bls.G1Point
		hash       *bls.Fr
		committer  Committer
	}

	Empty struct{}
)

func newInternalNode(depth int, cmtr Committer) VerkleNode {
	node := new(InternalNode)
	node.children = make([]VerkleNode, NodeWidth)
	for idx := range node.children {
		node.children[idx] = Empty(struct{}{})
	}
	node.depth = depth
	node.committer = cmtr
	node.count = 0
	return node
}

// New creates a new tree root
func New() VerkleNode {
	return newInternalNode(0, GetKZGConfig())
}

func (n *InternalNode) Depth() int {
	return n.depth
}

func (n *InternalNode) SetDepth(depth int) {
	n.depth = depth
}

func (n *InternalNode) Children() []VerkleNode {
	return n.children
}

func (n *InternalNode) SetChild(i int, c VerkleNode) error {
	if i >= NodeWidth-1 {
		return errors.New("child index higher than node width")
	}
	n.children[i] = c
	return nil
}

func (n *InternalNode) Insert(key []byte, value []byte, resolver NodeResolverFn) error {
	// Clear cached commitment on modification
	if n.commitment != nil {
		n.commitment = nil
		n.hash = nil
	}

	nChild := offset2key(key, n.depth)

	switch child := n.children[nChild].(type) {
	case Empty:
		lastNode := &LeafNode{
			key:       key,
			values:    make([][]byte, NodeWidth),
			committer: n.committer,
		}
		lastNode.values[key[31]] = value
		n.children[nChild] = lastNode
		n.count++
	case *HashedNode:
		if resolver != nil {
			hash := bls.FrTo32(child.ComputeCommitment())
			serialized, err := resolver(hash[:])
			if err != nil {
				return fmt.Errorf("verkle tree: error resolving node %x: %w", key, err)
			}
			resolved, err := ParseNode(serialized, n.depth+NodeBitWidth)
			if err != nil {
				return fmt.Errorf("verkle tree: error parsing resolved node %x: %w", key, err)
			}
			n.children[nChild] = resolved
			return n.children[nChild].Insert(key, value, resolver)
		}
		return errInsertIntoHash
	case *LeafNode:
		// Need to add a new branch node to differentiate
		// between two keys, if the keys are different.
		// Otherwise, just update the key.
		if equalPaths(child.key, key) {
			if err := child.Insert(key, value, resolver); err != nil {
				return err
			}
		} else {
			// A new branch node has to be inserted. Depending
			// on the next word in both keys, a recursion into
			// the moved leaf node can occur.
			nextWordInExistingKey := offset2key(child.key, n.depth+NodeBitWidth)
			newBranch := newInternalNode(n.depth+NodeBitWidth, n.committer).(*InternalNode)
			newBranch.count = 1
			n.children[nChild] = newBranch
			newBranch.children[nextWordInExistingKey] = child

			nextWordInInsertedKey := offset2key(key, n.depth+NodeBitWidth)
			if nextWordInInsertedKey != nextWordInExistingKey {
				// Next word differs, so this was the last level.
				// Insert it directly into its final slot.
				lastNode := &LeafNode{
					key:       key,
					values:    make([][]byte, NodeWidth),
					committer: n.committer,
				}
				lastNode.values[key[31]] = value
				newBranch.children[nextWordInInsertedKey] = lastNode
				newBranch.count++
			} else if err := newBranch.Insert(key, value, resolver); err != nil {
				return err
			}
		}
	default: // InternalNode
		return child.Insert(key, value, resolver)
	}
	return nil
}

func (n *InternalNode) toHashedNode() *HashedNode {
	return &HashedNode{n.hash, n.commitment}
}

func (n *InternalNode) InsertOrdered(key []byte, value []byte, flush NodeFlushFn) error {
	// Clear cached commitment on modification
	if n.commitment != nil {
		n.commitment = nil
		n.hash = nil
	}

	nChild := offset2key(key, n.depth)

	switch child := n.children[nChild].(type) {
	case Empty:
		// Insert into a new subtrie, which means that the
		// subtree directly preceding this new one, can
		// safely be calculated.
	searchFirstNonEmptyChild:
		for i := int(nChild) - 1; i >= 0; i-- {
			switch child := n.children[i].(type) {
			case Empty:
				continue
			case *LeafNode:
				digest := sha256.New()
				digest.Write(child.key[:31]) // Write the stem
				tmp := bls.FrTo32(child.ComputeCommitment())
				digest.Write(tmp[:])
				if flush != nil {
					flush(child)
				}
				hashToFr(child.hash, digest.Sum(nil))
				n.children[i] = child.toHashedNode()
				break searchFirstNonEmptyChild
			case *HashedNode:
				break searchFirstNonEmptyChild
			case *InternalNode:
				n.children[i].ComputeCommitment()
				if flush != nil {
					child.Flush(flush)
				}
				n.children[i] = child.toHashedNode()
				break searchFirstNonEmptyChild
			}
		}

		// NOTE: these allocations are inducing a noticeable slowdown
		lastNode := &LeafNode{
			key:       key,
			values:    make([][]byte, NodeWidth),
			committer: n.committer,
		}
		lastNode.values[key[31]] = value
		n.children[nChild] = lastNode
		n.count++

		// If the node was already created, then there was at least one
		// child. As a result, inserting this new leaf means there are
		// now more than one child in this node.
	case *HashedNode:
		return errInsertIntoHash
	case *LeafNode:
		// Need to add a new branch node to differentiate
		// between two keys, if the keys are different.
		// Otherwise, just update the key.
		if equalPaths(child.key, key) {
			child.values[key[31]] = value
		} else {
			// A new branch node has to be inserted. Depending
			// on the next word in both keys, a recursion into
			// the moved leaf node can occur.
			nextWordInExistingKey := offset2key(child.key, n.depth+NodeBitWidth)
			newBranch := newInternalNode(n.depth+NodeBitWidth, n.committer).(*InternalNode)
			newBranch.count = 1
			n.children[nChild] = newBranch

			nextWordInInsertedKey := offset2key(key, n.depth+NodeBitWidth)
			if nextWordInInsertedKey != nextWordInExistingKey {
				// Directly hash the (left) node that was already
				// inserted.
				digest := sha256.New()
				digest.Write(child.key[:31]) // Write the stem
				tmp := bls.FrTo32(child.ComputeCommitment())
				digest.Write(tmp[:])
				if flush != nil {
					flush(child)
				}
				hashToFr(child.hash, digest.Sum(nil))
				newBranch.children[nextWordInExistingKey] = child.toHashedNode()
				// Next word differs, so this was the last level.
				// Insert it directly into its final slot.
				lastNode := &LeafNode{
					key:       key,
					values:    make([][]byte, NodeWidth),
					committer: n.committer,
				}
				lastNode.values[key[31]] = value
				newBranch.children[nextWordInInsertedKey] = lastNode
				newBranch.count++
			} else {
				// Reinsert the leaf in order to recurse
				newBranch.children[nextWordInExistingKey] = child
				if err := newBranch.InsertOrdered(key, value, flush); err != nil {
					return err
				}
			}
		}
	default: // InternalNode
		return child.InsertOrdered(key, value, flush)
	}
	return nil
}

func (n *InternalNode) Delete(key []byte) error {
	// Clear cached commitment on modification
	n.commitment = nil
	n.hash = nil

	nChild := offset2key(key, n.depth)
	switch child := n.children[nChild].(type) {
	case Empty:
		return errDeleteNonExistent
	case *HashedNode:
		return errors.New("trying to delete from a hashed subtree")
	case *LeafNode:
		if !equalPaths(child.key, key) {
			return errDeleteNonExistent
		}
		if err := child.Delete(key); err != nil {
			return err
		}
		// Prune child if necessary
		for _, v := range child.values {
			if v != nil {
				// bail if at least one child node have been found
				return nil
			}
		}
		// leaf node is now empty, prune it
		n.children[nChild] = Empty{}
		n.count--
		return nil
	case *InternalNode:
		if err := child.Delete(key); err != nil {
			return err
		}
		// Prune child if necessary
		switch child.count {
		case 0:
			n.children[nChild] = Empty{}
			n.count--
		case 1:
			// child node has only one child, and if that child
			// is a LeafNode, then it needs to be removed since
			// its key is covered by the extension. Other nodes
			// with only one leaf could be the parent of a node
			// with more than one leaf, and so they must remain
			for i, v := range child.children {
				if _, ok := v.(*LeafNode); ok {
					n.children[nChild] = child.children[i]
					break
				}
			}
		default:
		}
	}
	return nil
}

// Flush hashes the children of an internal node and replaces them
// with HashedNode. It also sends the current node on the flush channel.
func (n *InternalNode) Flush(flush NodeFlushFn) {
	for i, child := range n.children {
		if c, ok := child.(*InternalNode); ok {
			c.Flush(flush)
			n.children[i] = c.toHashedNode()
		} else if c, ok := child.(*LeafNode); ok {
			flush(n.children[i])
			n.children[i] = c.toHashedNode()
		}
	}
	flush(n)
}

func (n *InternalNode) Get(k []byte, getter NodeResolverFn) ([]byte, error) {
	nChild := offset2key(k, n.depth)

	switch child := n.children[nChild].(type) {
	case Empty, nil:
		// Return nil as a signal that the value isn't
		// present in the tree. This matches the behavior
		// of SecureTrie in Geth.
		return nil, nil
	case *HashedNode:
		// if a resolution function is set, resolve the
		// current hash node.
		if getter == nil {
			return nil, errReadFromInvalid
		}

		commitment := bls.FrTo32(child.hash)
		payload, err := getter(commitment[:])
		if err != nil {
			return nil, err
		}

		// deserialize the payload and set it as the child
		c, err := ParseNode(payload, n.depth+NodeWidth)
		if err != nil {
			return nil, err
		}
		c.ComputeCommitment()
		n.children[nChild] = c

		return c.Get(k, getter)
	default: // InternalNode
		return child.Get(k, getter)
	}
}

var modulus *big.Int

func init() {
	var ok bool
	modulus, ok = big.NewInt(0).SetString("52435875175126190479447740508185965837690552500527637822603658699938581184513", 10)
	if !ok {
		panic("could not get modulus")
	}

}

// This function takes a hash and turns it into a bls.Fr integer, making
// sure that this doesn't overflow the modulus.
// This piece of code is really ugly, and probably a performance hog, it
// needs to be rewritten more efficiently.
func hashToFr(out *bls.Fr, h []byte) {
	// Q&D check that the hash doesn't exceed 32 bytes. hashToFr
	// should disappear in the short term, so a panic isn't a
	// big problem here.
	if len(h) != 32 {
		panic("invalid hash length ≠ 32")
	}
	h[31] &= 0x7F // mod 2^255

	// reverse endianness (little -> big)
	for i := 0; i < len(h)/2; i++ {
		h[i], h[len(h)-i-1] = h[len(h)-i-1], h[i]
	}

	// Apply modulus
	x := big.NewInt(0).SetBytes(h)
	x.Mod(x, modulus)

	// clear the buffer in case the trailing bytes were 0
	for i := 0; i < 32; i++ {
		h[i] = 0
	}

	// back to original endianness
	var processed [32]byte
	converted := x.Bytes()
	for i := 0; i < len(converted); i++ {
		processed[i] = converted[len(converted)-i-1]
	}

	if !bls.FrFrom32(out, processed) {
		panic(fmt.Sprintf("invalid Fr number %x", processed))
	}
	// TODO(@gballet) activate when geth moves to Go 1.17
	// in replacement of what is above.
	//if !bls.FrFrom32(out, (*[32]byte)(h)) {
	//panic(fmt.Sprintf("invalid Fr number %x", h))
	//}
}

func (n *InternalNode) ComputeCommitment() *bls.Fr {
	if n.hash != nil {
		return n.hash
	}
	n.hash = new(bls.Fr)

	emptyChildren := 0
	poly := make([]bls.Fr, NodeWidth)
	for idx, childC := range n.children {
		switch child := childC.(type) {
		case Empty:
			emptyChildren++
		case *LeafNode:
			// Store the leaf node hash in the polynomial, even if
			// the tree is free.
			digest := sha256.New()
			digest.Write(child.key[:31]) // Write the stem
			tmp := bls.FrTo32(child.ComputeCommitment())
			digest.Write(tmp[:])
			// special case: only one leaf node - then ignore the top
			// branch node.
			if n.count == 1 && n.depth == 0 {
				hashToFr(n.hash, digest.Sum(nil))
				// Set the commitment to nil, as there is no real commitment at this
				// level - only the hash has significance.
				n.commitment = nil
				return n.hash
			}
			hashToFr(&poly[idx], digest.Sum(nil))
		case *HashedNode:
			bls.CopyFr(&poly[idx], child.ComputeCommitment())
		default:
			childC.ComputeCommitment()
			bls.CopyFr(&poly[idx], child.ComputeCommitment())
		}
	}

	// All the coefficients have been computed, evaluate the polynomial,
	// serialize and hash the resulting point - this is the commitment.
	n.commitment = n.committer.CommitToPoly(poly, emptyChildren)
	serialized := bls.ToCompressedG1(n.commitment)
	h := sha256.Sum256(serialized)
	hashToFr(n.hash, h[:])

	return n.hash
}

func (n *InternalNode) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []uint, []*bls.Fr, [][]bls.Fr) {
	childIdx := offset2key(key, n.depth)

	// Special case, no commitment for the root if there is only one
	// child in the tree.
	if n.count == 1 {
		return n.children[childIdx].GetCommitmentsAlongPath(key)
	}

	comms, zis, yis, fis := n.children[childIdx].GetCommitmentsAlongPath(key)
	var yi bls.Fr
	fi := make([]bls.Fr, NodeWidth)
	for i, child := range n.children {
		if c, ok := child.(*LeafNode); ok {
			digest := sha256.New()
			digest.Write(c.key[:31]) // Write the stem
			tmp := bls.FrTo32(c.hash)
			digest.Write(tmp[:])
			hashToFr(&fi[i], digest.Sum(nil))
		} else {
			bls.CopyFr(&fi[i], child.ComputeCommitment())
		}

		if i == int(childIdx) {
			bls.CopyFr(&yi, &fi[i])
		}
	}
	return append(comms, n.commitment), append(zis, childIdx), append(yis, &yi), append(fis, fi)
}

func (n *InternalNode) Serialize() ([]byte, error) {
	var bitlist [128]uint8
	children := make([]byte, 0, NodeWidth*32)
	for i, c := range n.children {
		if _, ok := c.(Empty); !ok {
			setBit(bitlist[:], i)
			digits := bls.FrTo32(c.ComputeCommitment())
			children = append(children, digits[:]...)
		}
	}
	return rlp.EncodeToBytes([]interface{}{internalRLPType, bitlist, children})
}

func (n *InternalNode) Copy() VerkleNode {
	ret := &InternalNode{
		children:   make([]VerkleNode, len(n.children)),
		commitment: new(bls.G1Point),
		depth:      n.depth,
		committer:  n.committer,
		count:      n.count,
	}

	for i, child := range n.children {
		ret.children[i] = child.Copy()
	}

	if n.hash != nil {
		ret.hash = new(bls.Fr)
		bls.CopyFr(ret.hash, n.hash)
	}
	if n.commitment != nil {
		bls.CopyG1(ret.commitment, n.commitment)
	}

	return ret
}

// clearCache sets the commitment field of node
// and all of its children (recursively) to nil.
func (n *InternalNode) clearCache() {
	for _, c := range n.children {
		in, ok := c.(*InternalNode)
		if !ok {
			continue
		}
		in.clearCache()
	}
	n.commitment = nil
}

func (n *InternalNode) toDot(parent, path string) string {
	n.ComputeCommitment()
	me := fmt.Sprintf("internal%s", path)
	ret := fmt.Sprintf("%s [label=\"I: %x\"]\n", me, bls.FrTo32(n.hash))
	if len(parent) > 0 {
		ret = fmt.Sprintf("%s %s -> %s\n", ret, parent, me)
	}

	for i, child := range n.children {
		ret = fmt.Sprintf("%s%s", ret, child.toDot(me, fmt.Sprintf("%s%02x", path, i)))
	}

	return ret
}

func (n *LeafNode) toHashedNode() *HashedNode {
	return &HashedNode{n.hash, n.commitment}
}

func (n *LeafNode) Insert(k []byte, value []byte, _ NodeResolverFn) error {
	// Sanity check: ensure the key header is the same:
	if !equalPaths(k, n.key) {
		return errors.New("split should not happen here")
	}
	n.values[k[31]] = value
	n.commitment = nil
	n.hash = nil
	return nil
}

func (n *LeafNode) InsertOrdered(key []byte, value []byte, _ NodeFlushFn) error {
	// In the previous version, this value used to be flushed on insert.
	// This is no longer the case, as all values at the last level get
	// flushed at the same time.
	return n.Insert(key, value, nil)
}

func (n *LeafNode) Delete(k []byte) error {
	// Sanity check: ensure the key header is the same:
	if !equalPaths(k, n.key) {
		return errors.New("trying to delete a non-existing key")
	}

	n.commitment = nil
	n.hash = nil
	n.values[k[31]] = nil
	return nil
}

func (n *LeafNode) Get(k []byte, _ NodeResolverFn) ([]byte, error) {
	if !equalPaths(k, n.key) {
		// If keys differ, return nil in order to
		// signal that the key isn't present in the
		// tree. Do not return an error, thus matching
		// the behavior of Geth's SecureTrie.
		return nil, nil
	}
	// value can be nil, as expected by geth
	return n.values[k[31]], nil
}

func (n *LeafNode) ComputeCommitment() *bls.Fr {
	if n.hash != nil {
		return n.hash
	}
	n.hash = new(bls.Fr)

	emptyChildren := 0
	poly := make([]bls.Fr, NodeWidth)
	for idx, val := range n.values {
		if val == nil {
			emptyChildren++
			continue
		}
		h := sha256.Sum256(val)
		hashToFr(&poly[idx], h[:])
	}

	n.commitment = n.committer.CommitToPoly(poly, emptyChildren)

	h := sha256.Sum256(bls.ToCompressedG1(n.commitment))
	hashToFr(n.hash, h[:])
	return n.hash
}

func (n *LeafNode) GetCommitmentsAlongPath(key []byte) ([]*bls.G1Point, []uint, []*bls.Fr, [][]bls.Fr) {
	slot := uint64(key[31])
	fis := make([]bls.Fr, NodeWidth)
	for i, val := range n.values {
		if val != nil {
			var fi bls.Fr
			h := sha256.Sum256(val)
			hashToFr(&fi, h[:])
			bls.CopyFr(&fis[i], &fi)
		}
	}
	return []*bls.G1Point{n.commitment}, []uint{uint(slot)}, []*bls.Fr{&fis[slot]}, [][]bls.Fr{fis}
}

func (n *LeafNode) Serialize() ([]byte, error) {
	return rlp.EncodeToBytes([]interface{}{leafRLPType, n.key, n.values})
}

func (n *LeafNode) Copy() VerkleNode {
	l := &LeafNode{}
	l.key = make([]byte, len(n.key))
	l.values = make([][]byte, len(n.values))
	l.committer = n.committer
	copy(l.key, n.key)
	for i, v := range n.values {
		l.values[i] = make([]byte, len(v))
		copy(l.values[i], v)
	}
	if n.commitment != nil {
		l.commitment = n.commitment
	}
	if l.hash != nil {
		bls.CopyFr(l.hash, n.hash)
	}

	return l
}

func (n *LeafNode) Key(i int) []byte {
	var ret [32]byte
	copy(ret[:], n.key)
	ret[31] = byte(i)
	return ret[:]
}

func (n *LeafNode) Value(i int) []byte {
	return n.values[i]
}

func (n *LeafNode) toDot(parent, path string) string {
	return fmt.Sprintf("leaf%s [label=\"L: %x\"]\n%s -> leaf%s\n", path, bls.FrTo32(n.hash), parent, path)
}

func (*HashedNode) Insert([]byte, []byte, NodeResolverFn) error {
	return errInsertIntoHash
}

func (*HashedNode) InsertOrdered([]byte, []byte, NodeFlushFn) error {
	return errInsertIntoHash
}

func (*HashedNode) Delete([]byte) error {
	return errors.New("cant delete a hashed node in-place")
}

func (*HashedNode) Get([]byte, NodeResolverFn) ([]byte, error) {
	return nil, errors.New("can not read from a hash node")
}

func (n *HashedNode) ComputeCommitment() *bls.Fr {
	return n.hash
}

func (*HashedNode) GetCommitmentsAlongPath([]byte) ([]*bls.G1Point, []uint, []*bls.Fr, [][]bls.Fr) {
	panic("can not get the full path, and there is no proof of absence")
}

func (*HashedNode) Serialize() ([]byte, error) {
	return nil, errSerializeHashedNode
}

func (n *HashedNode) Copy() VerkleNode {
	h := &HashedNode{
		commitment: new(bls.G1Point),
		hash:       new(bls.Fr),
	}
	if n.hash != nil {
		bls.CopyFr(h.hash, n.hash)
	}
	if n.commitment != nil {
		bls.CopyG1(h.commitment, n.commitment)
	}

	return h
}

func (n *HashedNode) toDot(parent, path string) string {
	return fmt.Sprintf("hash%s [label=\"H: %x\"]\n%s -> hash%s\n", path, bls.FrTo32(n.hash), parent, path)
}

func (Empty) Insert([]byte, []byte, NodeResolverFn) error {
	return errors.New("an empty node should not be inserted directly into")
}

func (e Empty) InsertOrdered(key []byte, value []byte, _ NodeFlushFn) error {
	return e.Insert(key, value, nil)
}

func (Empty) Delete([]byte) error {
	return errors.New("cant delete an empty node")
}

func (Empty) Get([]byte, NodeResolverFn) ([]byte, error) {
	return nil, nil
}

func (Empty) ComputeCommitment() *bls.Fr {
	return &bls.ZERO
}

func (Empty) GetCommitmentsAlongPath([]byte) ([]*bls.G1Point, []uint, []*bls.Fr, [][]bls.Fr) {
	panic("trying to produce a commitment for an empty subtree")
}

func (Empty) Serialize() ([]byte, error) {
	return nil, errors.New("can't encode empty node to RLP")
}

func (Empty) Copy() VerkleNode {
	return Empty(struct{}{})
}

func (Empty) toDot(string, string) string {
	return ""
}

func setBit(bitlist []uint8, index int) {
	byt := index / 8
	bit := index % 8
	bitlist[byt] |= (uint8(1) << bit)
}

func ToDot(root VerkleNode) string {
	return fmt.Sprintf("digraph D {\n%s}", root.toDot("", ""))
}
