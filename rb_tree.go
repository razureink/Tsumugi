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
// （读者可能仍持有旧快照），因此不做池化——此前的"节点池"只有 Get 没有 Put，
// 实际从不复用，纯属开销。
func (t *llrbTree[K, V]) cloneNode(n *rbNode[K, V]) *rbNode[K, V] {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

type llrbTree[K cmp.Ordered, V any] struct {
	root atomic.Pointer[rbNode[K, V]]
	mu   sync.Mutex // 仅用于 writer 之间串行，reader 无锁
	size atomic.Int64
}

// newLLRB 构造左倾红黑树（键须为可排序类型，比较即内置 <）。
func newLLRB[K cmp.Ordered, V any]() *llrbTree[K, V] {
	return &llrbTree[K, V]{}
}

func (t *llrbTree[K, V]) Size() int64 { return t.size.Load() }

func (t *llrbTree[K, V]) isRed(x *rbNode[K, V]) bool { return x != nil && x.red }

func (t *llrbTree[K, V]) rotateLeft(h *rbNode[K, V]) *rbNode[K, V] {
	x := t.cloneNode(h.right)
	h = t.cloneNode(h)
	h.right = x.left
	x.left = h
	x.red = h.red
	h.red = true
	return x
}
func (t *llrbTree[K, V]) rotateRight(h *rbNode[K, V]) *rbNode[K, V] {
	x := t.cloneNode(h.left)
	h = t.cloneNode(h)
	h.left = x.right
	x.right = h
	x.red = h.red
	h.red = true
	return x
}

// flipColors toggle 三个节点的颜色（标准 LLRB 语义）。写时复制：返回克隆后的根。
func (t *llrbTree[K, V]) flipColors(h *rbNode[K, V]) *rbNode[K, V] {
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
func (t *llrbTree[K, V]) fixUp(h *rbNode[K, V]) *rbNode[K, V] {
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

func (t *llrbTree[K, V]) insert(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	h = t.cloneNode(h)
	var isNew bool
	// 记录哪一侧的子节点是本次写入"私有"的（递归返回或 nil→新建），
	// 私有节点可以原地改，共享节点改之前必须先克隆。
	leftOwned, rightOwned := false, false
	if key < h.key {
		h.left, isNew = t.insert(h.left, key, val)
		leftOwned = true
	} else if h.key < key {
		h.right, isNew = t.insert(h.right, key, val)
		rightOwned = true
	} else {
		h.value = val
		return h, false
	}
	return t.fixUpInsert(h, leftOwned, rightOwned), isNew
}

// insertNX 仅在 key 不存在时插入，不覆盖已有值。
func (t *llrbTree[K, V]) insertNX(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	if key == h.key {
		return h, false // 已存在：无需克隆路径（旧实现会先克隆整条路径再丢弃）
	}
	h = t.cloneNode(h)
	var isNew bool
	leftOwned, rightOwned := false, false
	if key < h.key {
		h.left, isNew = t.insertNX(h.left, key, val)
		leftOwned = true
	} else {
		h.right, isNew = t.insertNX(h.right, key, val)
		rightOwned = true
	}
	return t.fixUpInsert(h, leftOwned, rightOwned), isNew
}

// ==================== 插入路径：所有权感知的平衡修复 ====================
// 写时复制要求"改共享节点前先克隆"。但插入路径上本次自己克隆过的节点
// （以及递归返回的子节点）已经是私有的，再克隆一次纯属浪费——旧实现里
// 每次旋转/翻色都会把刚克隆的节点再克隆一遍，n 行写入约多出 1/3 的分配。
// 这里显式携带私有标记，只克隆真正共享的节点。

// rotateLeftOwned 把 h 的右子提升为子树根。h 已私有；右子若共享则克隆。
func (t *llrbTree[K, V]) rotateLeftOwned(h *rbNode[K, V], rightOwned bool) *rbNode[K, V] {
	x := h.right
	if !rightOwned {
		x = t.cloneNode(x)
	}
	h.right = x.left
	x.left = h
	x.red = h.red
	h.red = true
	return x
}

// rotateRightOwned 把 h 的左子提升为子树根。h 已私有；左子若共享则克隆。
func (t *llrbTree[K, V]) rotateRightOwned(h *rbNode[K, V], leftOwned bool) *rbNode[K, V] {
	x := h.left
	if !leftOwned {
		x = t.cloneNode(x)
	}
	h.left = x.right
	x.right = h
	x.red = h.red
	h.red = true
	return x
}

// flipColorsOwned 原地翻转 h 与子节点颜色；共享的子节点先克隆再翻。
func (t *llrbTree[K, V]) flipColorsOwned(h *rbNode[K, V], leftOwned, rightOwned bool) *rbNode[K, V] {
	h.red = !h.red
	if h.left != nil {
		if !leftOwned {
			h.left = t.cloneNode(h.left)
		}
		h.left.red = !h.left.red
	}
	if h.right != nil {
		if !rightOwned {
			h.right = t.cloneNode(h.right)
		}
		h.right.red = !h.right.red
	}
	return h
}

// fixUpInsert 插入路径的平衡修复（h 及其标记的子树均已私有）。
// 旋转/翻色后更新所有权标记，保证后续步骤不会误改共享节点。
func (t *llrbTree[K, V]) fixUpInsert(h *rbNode[K, V], leftOwned, rightOwned bool) *rbNode[K, V] {
	if t.isRed(h.right) && !t.isRed(h.left) {
		h = t.rotateLeftOwned(h, rightOwned)
		// 新根 x：左子 = 原来的 h（私有），右子 = 原 x.left（共享）
		leftOwned, rightOwned = true, false
	}
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRightOwned(h, leftOwned)
		// 新根 x：左子 = 原 x.right（共享），右子 = 原来的 h（私有）
		leftOwned, rightOwned = false, true
	}
	if t.isRed(h.left) && t.isRed(h.right) {
		h = t.flipColorsOwned(h, leftOwned, rightOwned)
		// 翻转后两个子节点都已私有
		leftOwned, rightOwned = true, true
	}
	return h
}

// Set 插入或覆盖，返回是否为新插入的键。
func (t *llrbTree[K, V]) Set(key K, val V) bool {
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
func (t *llrbTree[K, V]) SetNX(key K, val V) bool {
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

// ==================== 重放专用：原地可变操作 ====================
// 上面的 Set/Delete 采用写时复制：每次写入克隆 O(log n) 个节点，代价是与并发
// 读者无锁并行。WAL 重放阶段树尚未发布给任何读者（单 goroutine 独占），
// 因此可以原地修改，一次写入只分配"新增的那一个节点"——大表重放的分配量
// 从 O(n log n) 降到 O(n)，是加载时间的主要来源。
//
// 约束：只能在树没有任何并发读者/写者时调用（重放、批量导入）。发布之后
// 必须回到 COW 接口，否则会破坏读者持有的快照。

func (t *llrbTree[K, V]) rotateLeftMutable(h *rbNode[K, V]) *rbNode[K, V] {
	x := h.right
	h.right = x.left
	x.left = h
	x.red = h.red
	h.red = true
	return x
}

func (t *llrbTree[K, V]) rotateRightMutable(h *rbNode[K, V]) *rbNode[K, V] {
	x := h.left
	h.left = x.right
	x.right = h
	x.red = h.red
	h.red = true
	return x
}

// flipColorsMutable 原地翻转 h 与两个子节点的颜色（子节点可能为 nil）。
func (t *llrbTree[K, V]) flipColorsMutable(h *rbNode[K, V]) {
	h.red = !h.red
	if h.left != nil {
		h.left.red = !h.left.red
	}
	if h.right != nil {
		h.right.red = !h.right.red
	}
}

func (t *llrbTree[K, V]) fixUpMutable(h *rbNode[K, V]) *rbNode[K, V] {
	if t.isRed(h.right) && !t.isRed(h.left) {
		h = t.rotateLeftMutable(h)
	}
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRightMutable(h)
	}
	if t.isRed(h.left) && t.isRed(h.right) {
		t.flipColorsMutable(h)
	}
	return h
}

func (t *llrbTree[K, V]) insertMutable(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	var isNew bool
	if key < h.key {
		h.left, isNew = t.insertMutable(h.left, key, val)
	} else if h.key < key {
		h.right, isNew = t.insertMutable(h.right, key, val)
	} else {
		h.value = val
		return h, false
	}
	return t.fixUpMutable(h), isNew
}

func (t *llrbTree[K, V]) insertNXMutable(h *rbNode[K, V], key K, val V) (*rbNode[K, V], bool) {
	if h == nil {
		return &rbNode[K, V]{key: key, value: val, red: true}, true
	}
	var isNew bool
	if key < h.key {
		h.left, isNew = t.insertNXMutable(h.left, key, val)
	} else if h.key < key {
		h.right, isNew = t.insertNXMutable(h.right, key, val)
	} else {
		return h, false
	}
	return t.fixUpMutable(h), isNew
}

func (t *llrbTree[K, V]) moveRedLeftMutable(h *rbNode[K, V]) *rbNode[K, V] {
	t.flipColorsMutable(h)
	if h.right != nil && t.isRed(h.right.left) {
		h.right = t.rotateRightMutable(h.right)
		h = t.rotateLeftMutable(h)
		t.flipColorsMutable(h)
	}
	return h
}

func (t *llrbTree[K, V]) moveRedRightMutable(h *rbNode[K, V]) *rbNode[K, V] {
	t.flipColorsMutable(h)
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRightMutable(h)
		t.flipColorsMutable(h)
	}
	return h
}

func (t *llrbTree[K, V]) deleteMinMutable(h *rbNode[K, V]) *rbNode[K, V] {
	if h.left == nil {
		return nil
	}
	if !t.isRed(h.left) && !t.isRed(h.left.left) {
		h = t.moveRedLeftMutable(h)
	}
	h.left = t.deleteMinMutable(h.left)
	return t.fixUpMutable(h)
}

func (t *llrbTree[K, V]) deleteNodeMutable(h *rbNode[K, V], key K, removed *bool) *rbNode[K, V] {
	if h == nil {
		return nil
	}
	if key < h.key {
		if h.left == nil {
			return h
		}
		if !t.isRed(h.left) && !t.isRed(h.left.left) {
			h = t.moveRedLeftMutable(h)
		}
		h.left = t.deleteNodeMutable(h.left, key, removed)
	} else {
		if t.isRed(h.left) {
			h = t.rotateRightMutable(h)
		}
		if key == h.key && h.right == nil {
			*removed = true
			return nil
		}
		if h.right != nil {
			if !t.isRed(h.right) && !t.isRed(h.right.left) {
				h = t.moveRedRightMutable(h)
			}
			if key == h.key {
				minNode := t.min(h.right)
				h.key = minNode.key
				h.value = minNode.value
				*removed = true
				h.right = t.deleteMinMutable(h.right)
			} else {
				h.right = t.deleteNodeMutable(h.right, key, removed)
			}
		}
	}
	return t.fixUpMutable(h)
}

// SetMutable 原地插入/覆盖（重放专用），返回是否新键。
func (t *llrbTree[K, V]) SetMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root, isNew := t.insertMutable(t.root.Load(), key, val)
	if root != nil {
		root.red = false
	}
	t.root.Store(root)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

// SetNXMutable 原地"不存在才插入"（重放专用），返回是否插入成功。
func (t *llrbTree[K, V]) SetNXMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root, isNew := t.insertNXMutable(t.root.Load(), key, val)
	if !isNew {
		return false
	}
	if root != nil {
		root.red = false
	}
	t.root.Store(root)
	t.size.Add(1)
	return true
}

// DeleteMutable 原地删除（重放专用）。
func (t *llrbTree[K, V]) DeleteMutable(key K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.root.Load()
	if root == nil {
		return false
	}
	removed := false
	newRoot := t.deleteNodeMutable(root, key, &removed)
	if newRoot != nil {
		newRoot.red = false
	}
	t.root.Store(newRoot)
	if removed {
		t.size.Add(-1)
	}
	return removed
}

// UpdateMutable 原地覆盖已存在键的值；键不存在返回 false（不插入）。
// 重放 UPDATE 用它避免 insertMutable 的比较路径走两次。
func (t *llrbTree[K, V]) UpdateMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.root.Load()
	if root == nil {
		return false
	}
	h := root
	for h != nil {
		if key < h.key {
			h = h.left
		} else if h.key < key {
			h = h.right
		} else {
			h.value = val
			return true
		}
	}
	return false
}

func (t *llrbTree[K, V]) Get(key K) (V, bool) {
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

func (t *llrbTree[K, V]) Delete(key K) bool {
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
func (t *llrbTree[K, V]) deleteNode(h *rbNode[K, V], key K, removed *bool) *rbNode[K, V] {
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
func (t *llrbTree[K, V]) min(n *rbNode[K, V]) *rbNode[K, V] {
	for n.left != nil {
		n = n.left
	}
	return n
}
func (t *llrbTree[K, V]) moveRedLeft(h *rbNode[K, V]) *rbNode[K, V] {
	h = t.flipColors(h)
	if t.isRed(h.right.left) {
		h.right = t.rotateRight(h.right)
		h = t.rotateLeft(h)
		h = t.flipColors(h)
	}
	return h
}
func (t *llrbTree[K, V]) moveRedRight(h *rbNode[K, V]) *rbNode[K, V] {
	h = t.flipColors(h)
	if t.isRed(h.left) && t.isRed(h.left.left) {
		h = t.rotateRight(h)
		h = t.flipColors(h)
	}
	return h
}
func (t *llrbTree[K, V]) deleteMin(h *rbNode[K, V]) *rbNode[K, V] {
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

func (t *llrbTree[K, V]) scanAll(visit func(key K, value V)) {
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
func (t *llrbTree[K, V]) scanRangeStop(min, max *K, visit func(key K, value V) bool) {
	// 迭代式中序遍历：显式栈替代递归闭包，避免深树/大表扫描的调用开销。
	// 使用栈上固定数组避免每次查询的堆分配（64 深度覆盖所有实用场景）。
	root := t.root.Load()
	var stackBuf [64]*rbNode[K, V]
	stack := stackBuf[:0]
	n := root
	for n != nil || len(stack) > 0 {
		for n != nil {
			// n.key < min：该节点及整个左子树都小于下界，直接跳过左子树。
			// 旧实现只在弹出时过滤 < min 的节点，仍会先遍历 min 之前的
			// 全部节点；范围查询因此退化为从树头扫到 min。剪枝后只走
			// [min,max] 内节点及 O(log n) 查找路径。
			if min != nil && n.key < *min {
				n = n.right
				continue
			}
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
		// 入栈时已保证 n.key >= min；此处无需再过滤。
		if !visit(n.key, n.value) {
			return
		}
		n = n.right
	}
}

// scanRangeLimit 从 min 起按键升序扫描，凑够 limit 条即停止（返回是否扫满）。
func (t *llrbTree[K, V]) scanRangeLimit(min *K, limit int, visit func(key K, value V) bool) bool {
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
