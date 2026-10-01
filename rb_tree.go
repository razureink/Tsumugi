package main

import (
	"cmp"
	"sync"
	"sync/atomic"
)

// ==================== 通用左倾红黑树（LLRB） ====================
// 单一泛型实现同时服务主键索引（int64 → []byte）与二级索引（string → int64）。
// 采用标准 Sedgewick LLRB：颜色翻转 flipColors 是 toggle 语义（delete 依赖它维持不变量）。
//
// 键类型约束为 cmp.Ordered：比较走语言内置的 <（int64 是单条 CMP 指令，
// string 是直接的运行时比较），取代旧版经 less 闭包的间接调用——树高 ~17 时
// 一次 Get 需要两次函数指针调用 × 每层，是读路径上可观的固定开销。
//
// 并发模型：持久化 + 写时复制。所有修改函数只克隆受影响（位于改动路径上）的节点并
// 返回新子树，node 一旦被写入树后即视为不可变；writer 在写完新树后原子替换根指针。
// 因此 reader 只需原子加载一次根指针即可无锁遍历整棵（旧或新的）快照，读与读、
// 读与写之间完全无锁竞争，仅 writer 之间用一把互斥锁串行。

type rbNode[K any, V any] struct {
	key   K
	value V
	red   bool
	left  *rbNode[K, V]
	right *rbNode[K, V]
}

// cloneNode 浅拷贝节点（写时复制路径）。COW 语义下已发布节点不可回收复用
//（读者可能仍持有旧快照），因此不做池化——此前的"节点池"只有 Get 没有 Put，
// 实际从不复用，纯属开销。
func (t *RBTree[K, V]) cloneNode(n *rbNode[K, V]) *rbNode[K, V] {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

type RBTree[K cmp.Ordered, V any] struct {
	root atomic.Pointer[rbNode[K, V]]
	mu   sync.Mutex // 仅用于 writer 之间串行，reader 无锁
	size atomic.Int64
}

// NewRBTree 构造红黑树（键须为可排序类型，比较即内置 <）。
func NewRBTree[K cmp.Ordered, V any]() *RBTree[K, V] {
	return &RBTree[K, V]{}
}

// newIntKeyTree 构造以 int64 为键的树（主键索引）。
func newIntKeyTree[V any]() *RBTree[int64, V] {
	return NewRBTree[int64, V]()
}

// newStringKeyTree 构造以字符串为键的树（二级索引）。
// string 的 < 为逐字节字典序，与 bytes.Compare 排序语义一致。
func newStringKeyTree[V any]() *RBTree[string, V] {
	return NewRBTree[string, V]()
}

func (t *RBTree[K, V]) Size() int64 { return t.size.Load() }

func (t *RBTree[K, V]) isRed(x *rbNode[K, V]) bool { return x != nil && x.red }

func (t *RBTree[K, V]) rotateLeft(h *rbNode[K, V]) *rbNode[K, V] {
	x := t.cloneNode(h.right)
	h = t.cloneNode(h)
	h.right = x.left
	x.left = h
	x.red = h.red
	h.red = true
	return x
}
func (t *RBTree[K, V]) rotateRight(h *rbNode[K, V]) *rbNode[K, V] {
	x := t.cloneNode(h.left)
	h = t.cloneNode(h)
	h.left = x.right
	x.right = h
	x.red = h.red
	h.red = true
	return x
}

// flipColors toggle 三个节点的颜色（标准 LLRB 语义）。写时复制：返回克隆后的根。
func (t *RBTree[K, V]) flipColors(h *rbNode[K, V]) *rbNode[K, V] {
	h = t.cloneNode(h)
	h.red = !h.red
	if h.left != nil {
		h.left = t.cloneNode(h.left)
		h.left.red = !h.left.red
	}
	if h.right != nil {
		h.right = t.cloneNode(h.right)
		h.right.red = !h.right.red
	}
	return h
}
func (t *RBTree[K, V]) fixUp(h *rbNode[K, V]) *rbNode[K, V] {
	if t.isRed(h.right) && !t.isRed(h.left) {
		h = t.rotateLeft(h)
	}
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRight(h)
	}
	if t.isRed(h.left) && t.isRed(h.right) {
		h = t.flipColors(h)
	}
	return h
}

func (t *RBTree[K, V]) insert(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	h = t.cloneNode(h)
	var isNew bool
	if key < h.key {
		h.left, isNew = t.insert(h.left, key, val)
	} else if h.key < key {
		h.right, isNew = t.insert(h.right, key, val)
	} else {
		h.value = val
	}
	return t.fixUp(h), isNew
}

// insertNX 仅在 key 不存在时插入，不覆盖已有值。
func (t *RBTree[K, V]) insertNX(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	h = t.cloneNode(h)
	var isNew bool
	if key < h.key {
		h.left, isNew = t.insertNX(h.left, key, val)
	} else if h.key < key {
		h.right, isNew = t.insertNX(h.right, key, val)
	} else {
		return h, false // 已存在，保持原值（返回未增大的同一克隆，根未被替换）
	}
	return t.fixUp(h), isNew
}

// Set 插入或覆盖，返回是否为新插入的键。
func (t *RBTree[K, V]) Set(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root, isNew := t.insert(t.root.Load(), key, val)
	if root != nil {
		// insert/fixUp 返回的根必是本次调用新克隆的节点，尚未发布，可直接改色
		root.red = false
	}
	t.root.Store(root)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

// SetNX 原子地"不存在才插入"，返回是否成功插入。
func (t *RBTree[K, V]) SetNX(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	oldRoot := t.root.Load()
	root, isNew := t.insertNX(oldRoot, key, val)
	if !isNew {
		return false // 根不变（insertNX 克隆了路径但未实际改动键集合，仍须丢弃）
	}
	if root != nil {
		root.red = false // 同 Set：insertNX 返回的根是本次新克隆，可直接改色
	}
	t.root.Store(root)
	t.size.Add(1)
	return true
}

func (t *RBTree[K, V]) Get(key K) (V, bool) {
	h := t.root.Load()
	for h != nil {
		if key < h.key {
			h = h.left
		} else if h.key < key {
			h = h.right
		} else {
			return h.value, true
		}
	}
	return *new(V), false
}

func (t *RBTree[K, V]) Delete(key K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.root.Load()
	if root == nil {
		return false
	}
	removed := false
	// deleteNode 入口自克隆，此处无需先行克隆；返回根同为本次新克隆，可直接改色
	newRoot := t.deleteNode(root, key, &removed)
	if newRoot != nil {
		newRoot.red = false
	}
	t.root.Store(newRoot)
	if removed {
		t.size.Add(-1)
	}
	return removed
}
func (t *RBTree[K, V]) deleteNode(h *rbNode[K, V], key K, removed *bool) *rbNode[K, V] {
	if h == nil {
		return nil
	}
	h = t.cloneNode(h)
	if key < h.key {
		if h.left == nil {
			return h
		}
		if !t.isRed(h.left) && !t.isRed(h.left.left) {
			h = t.moveRedLeft(h)
		}
		h.left = t.deleteNode(h.left, key, removed)
	} else {
		if t.isRed(h.left) {
			h = t.rotateRight(h)
		}
		if key == h.key && h.right == nil {
			*removed = true
			return nil
		}
		if h.right != nil {
			if !t.isRed(h.right) && !t.isRed(h.right.left) {
				h = t.moveRedRight(h)
			}
			if key == h.key {
				minNode := t.min(h.right)
				h.key = minNode.key
				h.value = minNode.value
				*removed = true
				h.right = t.deleteMin(h.right)
			} else {
				h.right = t.deleteNode(h.right, key, removed)
			}
		}
	}
	return t.fixUp(h)
}
func (t *RBTree[K, V]) min(n *rbNode[K, V]) *rbNode[K, V] {
	for n.left != nil {
		n = n.left
	}
	return n
}
func (t *RBTree[K, V]) moveRedLeft(h *rbNode[K, V]) *rbNode[K, V] {
	h = t.flipColors(h)
	if t.isRed(h.right.left) {
		h.right = t.rotateRight(h.right)
		h = t.rotateLeft(h)
		h = t.flipColors(h)
	}
	return h
}
func (t *RBTree[K, V]) moveRedRight(h *rbNode[K, V]) *rbNode[K, V] {
	h = t.flipColors(h)
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRight(h)
		h = t.flipColors(h)
	}
	return h
}
func (t *RBTree[K, V]) deleteMin(h *rbNode[K, V]) *rbNode[K, V] {
	if h.left == nil {
		return nil
	}
	h = t.cloneNode(h)
	if !t.isRed(h.left) && !t.isRed(h.left.left) {
		h = t.moveRedLeft(h)
	}
	h.left = t.deleteMin(h.left)
	return t.fixUp(h)
}

func (t *RBTree[K, V]) scanAll(visit func(key K, value V)) {
	inorder(t.root.Load(), visit)
}
func inorder[K any, V any](n *rbNode[K, V], visit func(key K, value V)) {
	if n == nil {
		return
	}
	inorder(n.left, visit)
	visit(n.key, n.value)
	inorder(n.right, visit)
}

// scanRangeStop 与 scanRange 相同，但 visit 返回 bool：返回 false 立即停止遍历。
func (t *RBTree[K, V]) scanRangeStop(min, max *K, visit func(key K, value V) bool) {
	// 迭代式中序遍历：显式栈替代递归闭包，避免深树/大表扫描的调用开销。
	// 使用栈上固定数组避免每次查询的堆分配（64 深度覆盖所有实用场景）。
	root := t.root.Load()
	var stackBuf [64]*rbNode[K, V]
	stack := stackBuf[:0]
	n := root
	for n != nil || len(stack) > 0 {
		for n != nil {
			// n.key > max：该节点及其右子树整体超出范围，仅下钻左子树。
			if max != nil && *max < n.key {
				n = n.left
				continue
			}
			stack = append(stack, n)
			n = n.left
		}
		if len(stack) == 0 {
			break
		}
		n = stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		// n.key >= min 才访问；< min 时跳过本节点但继续右子树。
		if min == nil || !(n.key < *min) {
			if !visit(n.key, n.value) {
				return
			}
		}
		n = n.right
	}
}

// scanRangeLimit 从 min 起按键升序扫描，凑够 limit 条即停止（返回是否扫满）。
func (t *RBTree[K, V]) scanRangeLimit(min *K, limit int, visit func(key K, value V) bool) bool {
	root := t.root.Load()
	var stackBuf [64]*rbNode[K, V]
	stack := stackBuf[:0]
	n := root
	count := 0
	for n != nil || len(stack) > 0 {
		for n != nil {
			if min != nil && n.key < *min {
				n = n.right
				continue
			}
			stack = append(stack, n)
			n = n.left
		}
		if len(stack) == 0 {
			break
		}
		n = stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if min == nil || !(n.key < *min) {
			if !visit(n.key, n.value) {
				return true
			}
			count++
			if count >= limit {
				return true
			}
		}
		n = n.right
	}
	return count >= limit
}
