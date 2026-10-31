package ordered

import (
	"errors"
	"unsafe"
)

var ErrBudget = errors.New("ordered tree allocation budget exceeded")

// Key implements a stable total order. Keys and values remain immutable while
// any tree version or cursor can reach them; variable-width storage is borrowed.
type Key[K any] interface{ Compare(K) int }

type node[K Key[K], V any] struct {
	left, right *node[K, V]
	key         K
	value       V
	count       uint64
	height      uint8
}

// Tree is a persistent AVL tree. Copying it freezes a view without walking its
// entries. Updates copy only the search path and any rotated shared children.
// The zero value is an empty tree; independent versions need no reader locks.
type Tree[K Key[K], V any] struct{ root *node[K, V] }

func (t Tree[K, V]) Len() uint64 { return nodeCount(t.root) }

// NodeBytes counts nodes reachable from this version, excluding borrowed key and
// value payloads. Summing across versions can double-count shared nodes.
func (t Tree[K, V]) NodeBytes() uint64 {
	return nodeCount(t.root) * uint64(unsafe.Sizeof(node[K, V]{}))
}

func (t Tree[K, V]) Get(key K) (V, bool) {
	for current := t.root; current != nil; {
		order := key.Compare(current.key)
		if order < 0 {
			current = current.left
		} else if order > 0 {
			current = current.right
		} else {
			return current.value, true
		}
	}
	var zero V
	return zero, false
}

// Set and Delete charge new node allocations, including temporary rotation
// copies, before allocating. Input payload ownership stays with the caller.
func (t Tree[K, V]) Set(key K, value V, maxBytes uint64) (Tree[K, V], uint64, error) {
	budget := nodeBudget[K, V]{limit: maxBytes}
	root, err := budget.set(t.root, key, value)
	if err != nil {
		return t, budget.used, err
	}
	return Tree[K, V]{root: root}, budget.used, nil
}

func (t Tree[K, V]) Delete(key K, maxBytes uint64) (Tree[K, V], uint64, error) {
	budget := nodeBudget[K, V]{limit: maxBytes}
	root, err := budget.remove(t.root, key)
	if err != nil {
		return t, budget.used, err
	}
	return Tree[K, V]{root: root}, budget.used, nil
}

type nodeBudget[K Key[K], V any] struct{ limit, used uint64 }

func (b *nodeBudget[K, V]) clone(source *node[K, V]) (*node[K, V], error) {
	size := uint64(unsafe.Sizeof(node[K, V]{}))
	if size > b.limit-b.used {
		return nil, ErrBudget
	}
	b.used += size
	result := new(node[K, V])
	if source != nil {
		*result = *source
	}
	return result, nil
}

func nodeCount[K Key[K], V any](n *node[K, V]) uint64 {
	if n == nil {
		return 0
	}
	return n.count
}
func nodeHeight[K Key[K], V any](n *node[K, V]) int {
	if n == nil {
		return 0
	}
	return int(n.height)
}
func refresh[K Key[K], V any](n *node[K, V]) {
	n.count = 1 + nodeCount(n.left) + nodeCount(n.right)
	n.height = uint8(1 + max(nodeHeight(n.left), nodeHeight(n.right)))
}

func (b *nodeBudget[K, V]) set(source *node[K, V], key K, value V) (*node[K, V], error) {
	n, err := b.clone(source)
	if err != nil {
		return nil, err
	}
	if source == nil {
		n.key, n.value, n.count, n.height = key, value, 1, 1
		return n, nil
	}
	switch order := key.Compare(n.key); {
	case order < 0:
		n.left, err = b.set(n.left, key, value)
	case order > 0:
		n.right, err = b.set(n.right, key, value)
	default:
		n.value = value
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	return b.balance(n)
}

func (b *nodeBudget[K, V]) remove(source *node[K, V], key K) (*node[K, V], error) {
	if source == nil {
		return nil, nil
	}
	order := key.Compare(source.key)
	if order == 0 {
		if source.left == nil {
			return source.right, nil
		}
		if source.right == nil {
			return source.left, nil
		}
	}
	left, right := source.left, source.right
	replacementKey, replacementValue := source.key, source.value
	var err error
	switch {
	case order < 0:
		left, err = b.remove(left, key)
	case order > 0:
		right, err = b.remove(right, key)
	default:
		successor := right
		for successor.left != nil {
			successor = successor.left
		}
		replacementKey, replacementValue = successor.key, successor.value
		right, err = b.remove(right, successor.key)
	}
	if err != nil {
		return nil, err
	}
	if left == source.left && right == source.right {
		return source, nil
	}
	n, err := b.clone(source)
	if err != nil {
		return nil, err
	}
	n.left, n.right, n.key, n.value = left, right, replacementKey, replacementValue
	return b.balance(n)
}

func (b *nodeBudget[K, V]) balance(n *node[K, V]) (*node[K, V], error) {
	refresh(n)
	delta := nodeHeight(n.left) - nodeHeight(n.right)
	if delta > 1 {
		if nodeHeight(n.left.left) < nodeHeight(n.left.right) {
			child, err := b.clone(n.left)
			if err != nil {
				return nil, err
			}
			n.left, err = b.left(child)
			if err != nil {
				return nil, err
			}
		}
		return b.right(n)
	}
	if delta < -1 {
		if nodeHeight(n.right.right) < nodeHeight(n.right.left) {
			child, err := b.clone(n.right)
			if err != nil {
				return nil, err
			}
			n.right, err = b.right(child)
			if err != nil {
				return nil, err
			}
		}
		return b.left(n)
	}
	return n, nil
}

func (b *nodeBudget[K, V]) left(n *node[K, V]) (*node[K, V], error) {
	root, err := b.clone(n.right)
	if err != nil {
		return nil, err
	}
	n.right = root.left
	root.left = n
	refresh(n)
	refresh(root)
	return root, nil
}

func (b *nodeBudget[K, V]) right(n *node[K, V]) (*node[K, V], error) {
	root, err := b.clone(n.left)
	if err != nil {
		return nil, err
	}
	n.left = root.right
	root.right = n
	refresh(n)
	refresh(root)
	return root, nil
}

// A uint64-sized AVL tree has height below 128. The inline path avoids iterator
// allocations and releases consumed ancestors instead of retaining a root.
type Cursor[K Key[K], V any] struct {
	path    [128]*node[K, V]
	length  int
	reverse bool
}

func (t Tree[K, V]) First(reverse bool) Cursor[K, V] {
	c := Cursor[K, V]{reverse: reverse}
	c.descend(t.root)
	return c
}

// Seek includes an equal key; reverse selects the greatest key <= the bound.
func (t Tree[K, V]) Seek(key K, reverse bool) Cursor[K, V] {
	c := Cursor[K, V]{reverse: reverse}
	for n := t.root; n != nil; {
		order := n.key.Compare(key)
		eligible := order >= 0
		if reverse {
			eligible = order <= 0
		}
		if eligible {
			c.path[c.length] = n
			c.length++
			if reverse {
				n = n.right
			} else {
				n = n.left
			}
		} else if reverse {
			n = n.left
		} else {
			n = n.right
		}
	}
	return c
}

func (c *Cursor[K, V]) descend(n *node[K, V]) {
	for n != nil {
		c.path[c.length] = n
		c.length++
		if c.reverse {
			n = n.right
		} else {
			n = n.left
		}
	}
}

func (c *Cursor[K, V]) Next() (K, V, bool) {
	if c.length == 0 {
		var key K
		var value V
		return key, value, false
	}
	c.length--
	n := c.path[c.length]
	c.path[c.length] = nil
	if c.reverse {
		c.descend(n.left)
	} else {
		c.descend(n.right)
	}
	return n.key, n.value, true
}

func (c *Cursor[K, V]) Close() { clear(c.path[:c.length]); c.length = 0 }
