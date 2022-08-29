// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"sync/atomic"
	"unsafe"
)

// SAFTEY: The unsafe package is used, but with care.
// Using "unsafe" allows for one alloction per node and avoids having to use
// an interface{} type for child nodes; that may either be:
//   - *leafNode[T]
//   - *branchNode[T]
// This library makes it generally safe by guaranteeing that all references to
// nodes are simply to `*node[T]`, which is just the header struct for the leaf
// or branch representation. The difference between a leaf and a branch node is
// that a leaf has an array of item data of generic type T on tail of the
// struct, while a branch has an array of child node pointers on the tail. To
// access the child items `node[T].items()` is called; returning a slice, or
// nil if the node is a branch. To access the child nodes `node[T].children()`
// is called; returning a slice, or nil if the node is a leaf. The `items()`
// and `children()` methods check the `node[T].kind` to determine which kind of
// node it is, which is an enum of `none`, `leaf`, or `branch`. The only valid
// way to create a `*node[T]` is `RTreeG[T].newNode(leaf bool)` which take a
// bool that indicates the new node kind is a `leaf` or `branch`.

const maxEntries = 64
const minEntries = maxEntries * 10 / 100
const orderBranches = true
const orderLeaves = true
const quickChooser = false

// copy-on-write atomic incrementer
var cow uint64

type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64
}

type RTreeG2[N number, T any] struct {
	cow   uint64
	count int
	rect  rect[N]
	root  *node[N, T]
	empty T
}

type rect[N number] struct {
	min [2]N
	max [2]N
}

func (r *rect[N]) expand(b *rect[N]) {
	if b.min[0] < r.min[0] {
		r.min[0] = b.min[0]
	}
	if b.max[0] > r.max[0] {
		r.max[0] = b.max[0]
	}
	if b.min[1] < r.min[1] {
		r.min[1] = b.min[1]
	}
	if b.max[1] > r.max[1] {
		r.max[1] = b.max[1]
	}
}

type kind int8

const (
	none kind = iota
	leaf
	branch
)

type node[N number, T any] struct {
	cow   uint64
	kind  kind
	count int16
	rects [maxEntries]rect[N]
}

func (n *node[N, T]) leaf() bool {
	return n.kind == leaf
}

type leafNode[N number, T any] struct {
	node[N, T]
	items [maxEntries]T
}

type branchNode[N number, T any] struct {
	node[N, T]
	children [maxEntries]*node[N, T]
}

func (n *node[N, T]) children() []*node[N, T] {
	if n.kind != branch {
		// not a branch
		return nil
	}

	return (*branchNode[N, T])(unsafe.Pointer(n)).children[:]
}

func (n *node[N, T]) items() []T {
	if n.kind != leaf {
		// not a leaf
		return nil
	}
	return (*leafNode[N, T])(unsafe.Pointer(n)).items[:]
}

func (tr *RTreeG2[N, T]) newNode(isleaf bool) *node[N, T] {
	if isleaf {
		n := &leafNode[N, T]{node: node[N, T]{cow: tr.cow, kind: leaf}}
		return (*node[N, T])(unsafe.Pointer(n))
	} else {
		n := &branchNode[N, T]{node: node[N, T]{cow: tr.cow, kind: branch}}
		return (*node[N, T])(unsafe.Pointer(n))
	}
}

func (n *node[N, T]) rect() rect[N] {
	rect := n.rects[0]
	for i := 1; i < int(n.count); i++ {
		rect.expand(&n.rects[i])
	}
	return rect
}

// Insert data into tree
func (tr *RTreeG2[N, T]) Insert(min, max [2]N, data T) {
	ir := rect[N]{min, max}
	if tr.root == nil {
		tr.root = tr.newNode(true)
		tr.rect = ir
	}
	grown := tr.nodeInsert(&tr.rect, &tr.root, &ir, data)
	split := tr.root.count == maxEntries
	if grown {
		tr.rect.expand(&ir)
	}
	if split {
		left := tr.root
		right := tr.splitNode(tr.rect, left)
		tr.root = tr.newNode(false)
		tr.root.rects[0] = left.rect()
		tr.root.rects[1] = right.rect()
		tr.root.children()[0] = left
		tr.root.children()[1] = right
		tr.root.count = 2
	}
	if orderBranches && !tr.root.leaf() && (grown || split) {
		tr.root.sort()
	}
	tr.count++
}

func (tr *RTreeG2[N, T]) splitNode(r rect[N], left *node[N, T],
) (right *node[N, T]) {
	return tr.splitNodeLargestAxisEdgeSnap(r, left)
}

func (n *node[N, T]) orderToRight(idx int) int {
	for idx < int(n.count)-1 && n.rects[idx+1].min[0] < n.rects[idx].min[0] {
		n.swap(idx+1, idx)
		idx++
	}
	return idx
}

func (n *node[N, T]) orderToLeft(idx int) int {
	for idx > 0 && n.rects[idx].min[0] < n.rects[idx-1].min[0] {
		n.swap(idx, idx-1)
		idx--
	}
	return idx
}

// This operation should not be inlined because it's expensive and rarely
// called outside of heavy copy-on-write situations. Marking it "noinline"
// allows for the parent cowLoad to be inlined.
// go:noinline
func (tr *RTreeG2[N, T]) copy(n *node[N, T]) *node[N, T] {
	n2 := tr.newNode(n.leaf())
	*n2 = *n
	if n2.leaf() {
		copy(n2.items()[:n.count], n.items()[:n.count])
	} else {
		copy(n2.children()[:n.count], n.children()[:n.count])
	}
	return n2
}

// cowLoad loads the provided node and, if needed, performs a copy-on-write.
func (tr *RTreeG2[N, T]) cowLoad(cn **node[N, T]) *node[N, T] {
	if (*cn).cow != tr.cow {
		*cn = tr.copy(*cn)
	}
	return *cn
}

func (n *node[N, T]) rsearch(key N) int {
	rects := n.rects[:n.count]
	for i := 0; i < len(rects); i++ {
		if !(n.rects[i].min[0] < key) {
			return i
		}
	}
	return int(n.count)
}

func (n *node[N, T]) bsearch(key N) int {
	low, high := 0, int(n.count)
	for low < high {
		h := int(uint(low+high) >> 1)
		if !(key < n.rects[h].min[0]) {
			low = h + 1
		} else {
			high = h
		}
	}
	return low
}

func (tr *RTreeG2[N, T]) nodeInsert(nr *rect[N], cn **node[N, T],
	ir *rect[N], data T,
) (grown bool) {
	n := tr.cowLoad(cn)
	if n.leaf() {
		items := n.items()
		index := int(n.count)
		if orderLeaves {
			index = n.rsearch(ir.min[0])
			copy(n.rects[index+1:int(n.count)+1], n.rects[index:int(n.count)])
			copy(items[index+1:int(n.count)+1], items[index:int(n.count)])
		}
		n.rects[index] = *ir
		items[index] = data
		n.count++
		grown = !nr.contains(ir)
		return grown
	}

	// choose a subtree
	rects := n.rects[:n.count]
	index := -1
	var narea N
	// take a quick look for any nodes that contain the rect
	for i := 0; i < len(rects); i++ {
		if rects[i].contains(ir) {
			if quickChooser {
				index = i
				break
			} else {
				area := rects[i].area()
				if index == -1 || area < narea {
					index = i
					narea = area
				}
			}
		}
	}
	if index == -1 {
		index = n.chooseLeastEnlargement(ir)
	}

	children := n.children()
	grown = tr.nodeInsert(&n.rects[index], &children[index], ir, data)
	split := children[index].count == maxEntries
	if grown {
		// The child rectangle must expand to accomadate the new item.
		n.rects[index].expand(ir)
		if orderBranches {
			index = n.orderToLeft(index)
		}
		grown = !nr.contains(ir)
	}
	if split {
		left := children[index]
		right := tr.splitNode(n.rects[index], left)
		n.rects[index] = left.rect()
		if orderBranches {
			copy(n.rects[index+2:int(n.count)+1],
				n.rects[index+1:int(n.count)])
			copy(children[index+2:int(n.count)+1],
				children[index+1:int(n.count)])
			n.rects[index+1] = right.rect()
			children[index+1] = right
			n.count++
			if n.rects[index].min[0] > n.rects[index+1].min[0] {
				n.swap(index+1, index)
			}
			index++
			index = n.orderToRight(index)
		} else {
			n.rects[n.count] = right.rect()
			children[n.count] = right
			n.count++
		}

	}
	return grown
}

func (r *rect[N]) area() N {
	return (r.max[0] - r.min[0]) * (r.max[1] - r.min[1])
}

// contains return struct when b is fully contained inside of n
func (r *rect[N]) contains(b *rect[N]) bool {
	if b.min[0] < r.min[0] || b.max[0] > r.max[0] {
		return false
	}
	if b.min[1] < r.min[1] || b.max[1] > r.max[1] {
		return false
	}
	return true
}

// intersects returns true if both rects intersect each other.
func (r *rect[N]) intersects(b *rect[N]) bool {
	if b.min[0] > r.max[0] || b.max[0] < r.min[0] {
		return false
	}
	if b.min[1] > r.max[1] || b.max[1] < r.min[1] {
		return false
	}
	return true
}

func (n *node[N, T]) chooseLeastEnlargement(ir *rect[N]) (index int) {
	rects := n.rects[:int(n.count)]
	j := -1
	var jenlargement, jarea N
	for i := 0; i < len(rects); i++ {
		// calculate the enlarged area
		uarea := rects[i].unionedArea(ir)
		area := rects[i].area()
		enlargement := uarea - area
		if j == -1 || enlargement < jenlargement ||
			(!(enlargement > jenlargement) && area < jarea) {
			j, jenlargement, jarea = i, enlargement, area
		}
	}
	return j
}

func fmin[N number](a, b N) N {
	if a < b {
		return a
	}
	return b
}
func fmax[N number](a, b N) N {
	if a > b {
		return a
	}
	return b
}

// unionedArea returns the area of two rects expanded
func (r *rect[N]) unionedArea(b *rect[N]) N {
	return (fmax(r.max[0], b.max[0]) - fmin(r.min[0], b.min[0])) *
		(fmax(r.max[1], b.max[1]) - fmin(r.min[1], b.min[1]))
}

func (r rect[N]) largestAxis() (axis int) {
	if r.max[1]-r.min[1] > r.max[0]-r.min[0] {
		return 1
	}
	return 0
}

func (tr *RTreeG2[N, T]) splitNodeLargestAxisEdgeSnap(r rect[N],
	left *node[N, T],
) (right *node[N, T]) {
	axis := r.largestAxis()
	right = tr.newNode(left.leaf())
	for i := 0; i < int(left.count); i++ {
		minDist := left.rects[i].min[axis] - r.min[axis]
		maxDist := r.max[axis] - left.rects[i].max[axis]
		if minDist < maxDist {
			// stay left
		} else {
			// move to right
			tr.moveRectAtIndexInto(left, i, right)
			i--
		}
	}
	// Make sure that both left and right nodes have at least
	// minEntries by moving items into underflowed nodes.
	if left.count < minEntries {
		// reverse sort by min axis
		right.sortByAxis(axis, true, false)
		for left.count < minEntries {
			tr.moveRectAtIndexInto(right, int(right.count)-1, left)
		}
	} else if right.count < minEntries {
		// reverse sort by max axis
		left.sortByAxis(axis, true, true)
		for right.count < minEntries {
			tr.moveRectAtIndexInto(left, int(left.count)-1, right)
		}
	}

	if (orderBranches && !right.leaf()) || (orderLeaves && right.leaf()) {
		right.sort()
		// It's not uncommon that the left node is already ordered
		if !left.issorted() {
			left.sort()
		}
	}
	return right
}

func (tr *RTreeG2[N, T]) moveRectAtIndexInto(from *node[N, T], index int,
	into *node[N, T],
) {
	into.rects[into.count] = from.rects[index]
	from.rects[index] = from.rects[from.count-1]
	if from.leaf() {
		into.items()[into.count] = from.items()[index]
		from.items()[index] = from.items()[from.count-1]
		from.items()[from.count-1] = tr.empty
	} else {
		into.children()[into.count] = from.children()[index]
		from.children()[index] = from.children()[from.count-1]
		from.children()[from.count-1] = nil
	}
	from.count--
	into.count++
}

func (n *node[N, T]) search(target rect[N],
	iter func(min, max [2]N, data T) bool,
) bool {
	rects := n.rects[:n.count]
	if n.leaf() {
		items := n.items()
		for i := 0; i < len(rects); i++ {
			if rects[i].intersects(&target) {
				if !iter(rects[i].min, rects[i].max, items[i]) {
					return false
				}
			}
		}
		return true
	}
	children := n.children()
	for i := 0; i < len(rects); i++ {
		if target.intersects(&rects[i]) {
			if !children[i].search(target, iter) {
				return false
			}
		}
	}
	return true
}

// Len returns the number of items in tree
func (tr *RTreeG2[N, T]) Len() int {
	return tr.count
}

// Search for items in tree that intersect the provided rectangle
func (tr *RTreeG2[N, T]) Search(min, max [2]N,
	iter func(min, max [2]N, data T) bool,
) {
	target := rect[N]{min, max}
	if tr.root == nil {
		return
	}
	if target.intersects(&tr.rect) {
		tr.root.search(target, iter)
	}
}

// Scane all items in the tree
func (tr *RTreeG2[N, T]) Scan(iter func(min, max [2]N, data T) bool) {
	if tr.root != nil {
		tr.root.scan(iter)
	}
}

func (n *node[N, T]) scan(iter func(min, max [2]N, data T) bool) bool {
	if n.leaf() {
		for i := 0; i < int(n.count); i++ {
			if !iter(n.rects[i].min, n.rects[i].max, n.items()[i]) {
				return false
			}
		}
	} else {
		for i := 0; i < int(n.count); i++ {
			if !n.children()[i].scan(iter) {
				return false
			}
		}
	}
	return true
}

// Copy the tree.
// This is a copy-on-write operation and is very fast because it only performs
// a shadowed copy.
func (tr *RTreeG2[N, T]) Copy() *RTreeG2[N, T] {
	tr2 := new(RTreeG2[N, T])
	*tr2 = *tr
	tr.cow = atomic.AddUint64(&cow, 1)
	tr2.cow = atomic.AddUint64(&cow, 1)
	return tr2
}

// swap two rectanlges
func (n *node[N, T]) swap(i, j int) {
	n.rects[i], n.rects[j] = n.rects[j], n.rects[i]
	if n.leaf() {
		n.items()[i], n.items()[j] = n.items()[j], n.items()[i]
	} else {
		n.children()[i], n.children()[j] = n.children()[j], n.children()[i]
	}
}

func (n *node[N, T]) sortByAxis(axis int, rev, max bool) {
	n.qsort(0, int(n.count), axis, rev, max)
}

func (n *node[N, T]) sort() {
	n.qsort(0, int(n.count), 0, false, false)
}

func (n *node[N, T]) issorted() bool {
	rects := n.rects[:n.count]
	for i := 1; i < len(rects); i++ {
		if rects[i].min[0] < rects[i-1].min[0] {
			return false
		}
	}
	return true
}

func (n *node[N, T]) qsort(s, e int, axis int, rev, max bool) {
	nrects := e - s
	if nrects < 2 {
		return
	}
	left, right := 0, nrects-1
	pivot := nrects / 2 // rand and mod not worth it
	n.swap(s+pivot, s+right)
	rects := n.rects[s:e]
	if !rev {
		if !max {
			for i := 0; i < len(rects); i++ {
				if rects[i].min[axis] < rects[right].min[axis] {
					n.swap(s+i, s+left)
					left++
				}
			}
		} else {
			for i := 0; i < len(rects); i++ {
				if rects[i].max[axis] < rects[right].max[axis] {
					n.swap(s+i, s+left)
					left++
				}
			}
		}
	} else {
		if !max {
			for i := 0; i < len(rects); i++ {
				if rects[right].min[axis] < rects[i].min[axis] {
					n.swap(s+i, s+left)
					left++
				}
			}
		} else {
			for i := 0; i < len(rects); i++ {
				if rects[right].max[axis] < rects[i].max[axis] {
					n.swap(s+i, s+left)
					left++
				}
			}
		}
	}
	n.swap(s+left, s+right)
	n.qsort(s, s+left, axis, rev, max)
	n.qsort(s+left+1, e, axis, rev, max)
}

// Delete data from tree
func (tr *RTreeG2[N, T]) Delete(min, max [2]N, data T) {
	tr.delete(min, max, data)
}

func (tr *RTreeG2[N, T]) delete(min, max [2]N, data T) bool {
	ir := rect[N]{min, max}
	if tr.root == nil || !tr.rect.contains(&ir) {
		return false
	}
	var reinsert []*node[N, T]
	removed, _ := tr.nodeDelete(&tr.rect, &tr.root, &ir, data, &reinsert)
	if !removed {
		return false
	}
	tr.count--
	if len(reinsert) > 0 {
		for _, n := range reinsert {
			tr.count -= n.deepCount()
		}
	}
	if tr.count == 0 {
		tr.root = nil
		tr.rect.min = [2]N{0, 0}
		tr.rect.max = [2]N{0, 0}
	} else {
		for !tr.root.leaf() && tr.root.count == 1 {
			tr.root = tr.root.children()[0]
		}
	}
	if len(reinsert) > 0 {
		for i := range reinsert {
			tr.nodeReinsert(reinsert[i])
		}
	}
	return true
}

func compare[T any](a, b T) bool {
	return (interface{})(a) == (interface{})(b)
}

func (tr *RTreeG2[N, T]) nodeDelete(nr *rect[N], cn **node[N, T], ir *rect[N],
	data T, reinsert *[]*node[N, T],
) (removed, shrunk bool) {
	n := tr.cowLoad(cn)
	rects := n.rects[:n.count]
	if n.leaf() {
		items := n.items()
		for i := 0; i < len(rects); i++ {
			if ir.contains(&rects[i]) && compare(items[i], data) {
				// found the target item to delete
				if orderLeaves {
					copy(n.rects[i:n.count], n.rects[i+1:n.count])
					copy(items[i:n.count], items[i+1:n.count])
				} else {
					n.rects[i] = n.rects[n.count-1]
					items[i] = items[n.count-1]
				}
				items[len(rects)-1] = tr.empty
				n.count--
				shrunk = ir.onedge(nr)
				if shrunk {
					*nr = n.rect()
				}
				return true, shrunk
			}
		}
		return false, false
	}
	children := n.children()
	for i := 0; i < len(rects); i++ {
		if !rects[i].contains(ir) {
			continue
		}
		crect := rects[i]
		removed, shrunk = tr.nodeDelete(&rects[i], &children[i], ir, data,
			reinsert)
		if !removed {
			continue
		}
		if children[i].count < minEntries {
			*reinsert = append(*reinsert, children[i])
			if orderBranches {
				copy(n.rects[i:n.count], n.rects[i+1:n.count])
				copy(children[i:n.count], children[i+1:n.count])
			} else {
				n.rects[i] = n.rects[n.count-1]
				children[i] = children[n.count-1]
			}
			children[n.count-1] = nil
			n.count--
			*nr = n.rect()
			return true, true
		}
		if shrunk {
			shrunk = !rects[i].equals(&crect)
			if shrunk {
				*nr = n.rect()
			}
			if orderBranches {
				i = n.orderToRight(i)
			}
		}
		return true, shrunk
	}
	return false, false
}

func (r *rect[N]) equals(b *rect[N]) bool {
	return !(r.min[0] < b.min[0] || r.min[0] > b.min[0] ||
		r.min[1] < b.min[1] || r.min[1] > b.min[1] ||
		r.max[0] < b.max[0] || r.max[0] > b.max[0] ||
		r.max[1] < b.max[1] || r.max[1] > b.max[1])
}

type reinsertItem2[N number, T any] struct {
	rect rect[N]
	data T
}

func (n *node[N, T]) deepCount() int {
	if n.leaf() {
		return int(n.count)
	}
	var count int
	children := n.children()[:n.count]
	for i := 0; i < len(children); i++ {
		count += children[i].deepCount()
	}
	return count
}

func (tr *RTreeG2[N, T]) nodeReinsert(n *node[N, T]) {
	if n.leaf() {
		rects := n.rects[:n.count]
		items := n.items()[:n.count]
		for i := range rects {
			tr.Insert(rects[i].min, rects[i].max, items[i])
		}
	} else {
		children := n.children()[:n.count]
		for i := 0; i < len(children); i++ {
			tr.nodeReinsert(children[i])
		}
	}
}

// onedge returns true when r is on the edge of b
func (r *rect[N]) onedge(b *rect[N]) bool {
	return !(r.min[0] > b.min[0] && r.min[1] > b.min[1] &&
		r.max[0] < b.max[0] && r.max[1] < b.max[1])
}

// Replace an item.
// If the old item does not exist then the new item is not inserted.
func (tr *RTreeG2[N, T]) Replace(
	oldMin, oldMax [2]N, oldData T,
	newMin, newMax [2]N, newData T,
) {
	if tr.delete(oldMin, oldMax, oldData) {
		tr.Insert(newMin, newMax, newData)
	}
}

// Bounds returns the minimum bounding rect
func (tr *RTreeG2[N, T]) Bounds() (min, max [2]N) {
	return tr.rect.min, tr.rect.max
}
