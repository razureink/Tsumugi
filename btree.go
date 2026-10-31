package main

// ==================== 写时复制 B 树（btree） ====================
// 与左倾红黑树（llrbTree）提供完全相同的语义与并发模型，仅平衡结构不同：
//
//   - 红黑树是二叉树，树高 ≈ 2·log₂(n)。写时复制每次写入要克隆整条路径，
//     100 万行时约 22 层 → 22 个节点分配；
//   - B 树是多叉树，树高 ≈ log_B(n)。分支因子 8 时 100 万行约 7 层 → 7 个分配。
//
// 读路径的"无锁快照"完全保留：节点一经发布即视为不可变，写者克隆路径后
// 原子替换根指针，读者只加载一次根指针即可安全遍历旧快照。
//
// 节点用定长数组而非切片：克隆一个节点只需一次结构体拷贝 = 一次分配。
// 若用切片，每克隆一层要额外分配 keys/vals/kids 三块，前功尽弃。

import (
	"cmp"
	"sync"
	"sync/atomic"
)

const (
	// btMaxKeys 单节点最多键数（必须为奇数，保证分裂后两半都不低于 btMinKeys）；分支因子 = btMaxKeys+1。
	// 调大 → 层数更少、单次克隆字节更多；调小则相反。
	btMaxKeys = 5
	// btMinKeys 非根节点最少键数（btMaxKeys/2），低于它就借位或合并。
	btMinKeys = 2
)

type btNode[K cmp.Ordered, V any] struct {
	n    int
	keys [btMaxKeys]K
	vals [btMaxKeys]V
	kids [btMaxKeys + 1]*btNode[K, V]
}

// isLeaf 叶子节点没有子节点（kids 全为 nil）。
func (n *btNode[K, V]) isLeaf() bool { return n.kids[0] == nil }

type btree[K cmp.Ordered, V any] struct {
	root atomic.Pointer[btNode[K, V]]
	mu   sync.Mutex // 仅 writer 之间串行；reader 无锁
	size atomic.Int64
}

// newBTree 构造写时复制 B 树。
func newBTree[K cmp.Ordered, V any]() *btree[K, V] { return &btree[K, V]{} }

func (t *btree[K, V]) Size() int64 { return t.size.Load() }

// clone 浅拷贝节点（一次分配）。发布后的节点永不修改。
func (t *btree[K, V]) clone(n *btNode[K, V]) *btNode[K, V] {
	c := *n
	return &c
}

// btLowerBound 返回第一个 keys[i] >= key 的下标。
func btLowerBound[K cmp.Ordered, V any](n *btNode[K, V], key K) int {
	lo, hi := 0, n.n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if n.keys[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// ---------------- 读 ----------------

func (t *btree[K, V]) Get(key K) (V, bool) {
	n := t.root.Load()
	for n != nil {
		i := btLowerBound(n, key)
		if i < n.n && n.keys[i] == key {
			return n.vals[i], true
		}
		if n.isLeaf() {
			break
		}
		n = n.kids[i]
	}
	var zero V
	return zero, false
}

// ---------------- 写 ----------------

func (t *btree[K, V]) Set(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	isNew := t.setLocked(key, val, true, false)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

func (t *btree[K, V]) SetNX(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	isNew := t.setLocked(key, val, true, true)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

// setLocked 插入/覆盖。cow 为 true 时克隆路径（并发安全），
// false 时为原地修改（仅限 WAL 重放这类独占场景）。
func (t *btree[K, V]) setLocked(key K, val V, cow, nxOnly bool) bool {
	root := t.root.Load()
	if root == nil {
		n := &btNode[K, V]{n: 1}
		n.keys[0], n.vals[0] = key, val
		t.root.Store(n)
		return true
	}
	if root.n >= btMaxKeys {
		// 根满：新建根并把旧根作为唯一子节点再分裂（标准 B 树增高方式）
		base := root
		if cow {
			base = t.clone(root)
		}
		newRoot := &btNode[K, V]{}
		newRoot.kids[0] = base
		t.splitChild(newRoot, 0, true) // base 已私有
		root = newRoot
	}
	isNew := t.insert(root, key, val, cow, nxOnly)
	t.root.Store(root)
	return isNew
}

// insert 递归插入。n 在 cow 模式下已被克隆（私有），可安全修改。
func (t *btree[K, V]) insert(n *btNode[K, V], key K, val V, cow, nxOnly bool) bool {
	i := btLowerBound(n, key)
	if i < n.n && n.keys[i] == key {
		if nxOnly {
			return false
		}
		n.vals[i] = val
		return false
	}
	if n.isLeaf() {
		t.insertKeyAt(n, i, key, val)
		return true
	}
	if n.kids[i].n >= btMaxKeys {
		// 下降前先把满子节点分裂（自上而下，避免回溯修复）
		t.splitChild(n, i, !cow)
		i = btLowerBound(n, key)
		if i < n.n && n.keys[i] == key {
			if nxOnly {
				return false
			}
			n.vals[i] = val
			return false
		}
	}
	child := n.kids[i]
	if cow {
		child = t.clone(child)
		n.kids[i] = child
	}
	return t.insert(child, key, val, cow, nxOnly)
}

// insertKeyAt 在叶子节点的 i 位置插入键值。
func (t *btree[K, V]) insertKeyAt(n *btNode[K, V], i int, key K, val V) {
	copy(n.keys[i+1:], n.keys[i:n.n])
	copy(n.vals[i+1:], n.vals[i:n.n])
	n.keys[i], n.vals[i] = key, val
	n.n++
}

// removeKeyAt 从节点 i 位置删除键值。
func (t *btree[K, V]) removeKeyAt(n *btNode[K, V], i int) {
	copy(n.keys[i:], n.keys[i+1:n.n])
	copy(n.vals[i:], n.vals[i+1:n.n])
	n.n--
}

// splitChild 把已满的 kids[i] 分裂成两个，中位键上移到 n。
// n 必须已私有；childOwned 表示 kids[i] 是否已经是本次写入私有节点。
func (t *btree[K, V]) splitChild(n *btNode[K, V], i int, childOwned bool) {
	child := n.kids[i]
	if !childOwned {
		child = t.clone(child)
	}
	mid := btMaxKeys / 2 // 7/2 = 3：左 3 键、中位 1 键、右 3 键
	right := &btNode[K, V]{n: btMaxKeys - mid - 1}
	copy(right.keys[:], child.keys[mid+1:btMaxKeys])
	copy(right.vals[:], child.vals[mid+1:btMaxKeys])
	copy(right.kids[:], child.kids[mid+1:btMaxKeys+1])
	child.n = mid

	// n 腾出 i 位置（先移位，再写入中位键，顺序不能反）
	copy(n.keys[i+1:], n.keys[i:n.n])
	copy(n.vals[i+1:], n.vals[i:n.n])
	copy(n.kids[i+2:], n.kids[i+1:n.n+1])
	n.keys[i], n.vals[i] = child.keys[mid], child.vals[mid]
	n.kids[i] = child
	n.kids[i+1] = right
	n.n++
}

func (t *btree[K, V]) Delete(key K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.root.Load()
	if root == nil {
		return false
	}
	root = t.clone(root)
	root, removed := t.remove(root, key, true)
	if removed {
		// 收缩只能在这里做一次：中间层节点不会变成空节点（B 树最少键数约束），
		// 只有根可能因为合并而变成"0 键 + 单子节点"的退化形态。
		root = t.shrinkRoot(root)
		t.root.Store(root)
		t.size.Add(-1)
	} else {
		t.root.Store(root)
	}
	return removed
}

// remove 删除 key。n 已私有。采用自上而下的"预先补足"策略：
// 下降前保证目标子节点至少有 btMinKeys+1 个键，这样删除后不会下溢，
// 也就不需要回溯修复（回溯在写时复制下会额外克隆大量节点）。
func (t *btree[K, V]) remove(n *btNode[K, V], key K, cow bool) (*btNode[K, V], bool) {
	for {
		i := btLowerBound(n, key)
		if i < n.n && n.keys[i] == key {
			if n.isLeaf() {
				t.removeKeyAt(n, i)
				return n, true
			}
			// 内部节点命中：先补齐左子，再用左子树最大键替换本键，
			// 最后到左子树里真正删除那个前驱键。
			var merged bool
			n, i, merged = t.ensureChild(n, i, cow)
			if merged {
				// 键被合并下移了一层，重新定位后继续（每轮至少下降一层）
				continue
			}
			// 向右兄弟借位会把父键（可能正是待删键）下移进 kids[i]。
			// 此时它已不在本节点，必须改为直接到子树里删除——否则下面
			// "取前驱写回父节点"会把这个键又写回来，表现为
			// "Delete 返回成功但键仍在树里"。
			if j := btLowerBound(n, key); j >= n.n || n.keys[j] != key {
				child, removed := t.remove(n.kids[j], key, cow)
				n.kids[j] = child
				return n, removed
			}
			pk, pv := t.maxKey(n.kids[i])
			n.keys[i], n.vals[i] = pk, pv
			child, _ := t.remove(n.kids[i], pk, cow)
			n.kids[i] = child
			return n, true
		}
		if n.isLeaf() {
			return n, false
		}
		var merged bool
		n, i, merged = t.ensureChild(n, i, cow)
		if merged {
			continue // 目标键可能被下移，重新定位
		}
		child, removed := t.remove(n.kids[i], key, cow)
		n.kids[i] = child
		if !removed {
			return n, false
		}
		return n, true
	}
}

// shrinkRoot 根节点空了且还有子节点时把子节点提升为新根（树高降低一层）。
func (t *btree[K, V]) shrinkRoot(n *btNode[K, V]) *btNode[K, V] {
	if n.n == 0 && !n.isLeaf() {
		return n.kids[0]
	}
	return n
}

// maxKey 返回子树中的最大键值（沿最右路径下到底）。
func (t *btree[K, V]) maxKey(n *btNode[K, V]) (K, V) {
	for !n.isLeaf() {
		n = n.kids[n.n]
	}
	return n.keys[n.n-1], n.vals[n.n-1]
}

// ensureChild 保证 kids[i] 至少有 btMinKeys+1 个键（借位或合并）。
// 返回 (可能就地修改过的 n, 有效下标, 是否发生了合并)。
func (t *btree[K, V]) ensureChild(n *btNode[K, V], i int, cow bool) (*btNode[K, V], int, bool) {
	if n.kids[i].n > btMinKeys {
		return n, i, false
	}
	self := n.kids[i]
	if cow {
		self = t.clone(self)
		n.kids[i] = self
	}
	// 1) 向左兄弟借
	if i > 0 {
		sib := n.kids[i-1]
		if cow {
			sib = t.clone(sib)
			n.kids[i-1] = sib
		}
		if sib.n > btMinKeys {
			t.rotateFromLeft(n, i, self, sib)
			return n, i, false
		}
	}
	// 2) 向右兄弟借
	if i < n.n {
		sib := n.kids[i+1]
		if cow {
			sib = t.clone(sib)
			n.kids[i+1] = sib
		}
		if sib.n > btMinKeys {
			t.rotateFromRight(n, i, self, sib)
			return n, i, false
		}
	}
	// 3) 合并（两侧兄弟都只有 btMinKeys）
	if i > 0 {
		t.mergeAt(n, i-1)
		return n, i - 1, true
	}
	t.mergeAt(n, 0)
	return n, 0, true
}

// rotateFromLeft 把左兄弟的最后一个键经父键旋转到 self 的首位。
func (t *btree[K, V]) rotateFromLeft(n *btNode[K, V], i int, self, sib *btNode[K, V]) {
	copy(self.keys[1:], self.keys[:self.n])
	copy(self.vals[1:], self.vals[:self.n])
	copy(self.kids[1:], self.kids[:self.n+1])
	self.keys[0], self.vals[0] = n.keys[i-1], n.vals[i-1]
	if !sib.isLeaf() {
		self.kids[0] = sib.kids[sib.n]
	}
	self.n++
	n.keys[i-1], n.vals[i-1] = sib.keys[sib.n-1], sib.vals[sib.n-1]
	sib.n--
}

// rotateFromRight 把右兄弟的第一个键经父键旋转到 self 末尾。
func (t *btree[K, V]) rotateFromRight(n *btNode[K, V], i int, self, sib *btNode[K, V]) {
	self.keys[self.n], self.vals[self.n] = n.keys[i], n.vals[i]
	if !sib.isLeaf() {
		self.kids[self.n+1] = sib.kids[0]
	}
	self.n++
	n.keys[i], n.vals[i] = sib.keys[0], sib.vals[0]
	copy(sib.keys[:], sib.keys[1:sib.n])
	copy(sib.vals[:], sib.vals[1:sib.n])
	copy(sib.kids[:], sib.kids[1:sib.n+1])
	sib.n--
}

// mergeAt 把 kids[j]、keys[j]、kids[j+1] 合并进 kids[j]。
// 两个子节点都必须已经是私有节点（调用方负责克隆）。
func (t *btree[K, V]) mergeAt(n *btNode[K, V], j int) {
	left, right := n.kids[j], n.kids[j+1]
	left.keys[left.n], left.vals[left.n] = n.keys[j], n.vals[j]
	copy(left.keys[left.n+1:], right.keys[:right.n])
	copy(left.vals[left.n+1:], right.vals[:right.n])
	if !right.isLeaf() {
		copy(left.kids[left.n+1:], right.kids[:right.n+1])
	}
	left.n += 1 + right.n

	copy(n.keys[j:], n.keys[j+1:n.n])
	copy(n.vals[j:], n.vals[j+1:n.n])
	copy(n.kids[j+1:], n.kids[j+2:n.n+1])
	n.n--
}

// ---------------- 重放专用：原地修改 ----------------

func (t *btree[K, V]) SetMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	isNew := t.setLocked(key, val, false, false)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

func (t *btree[K, V]) SetNXMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	isNew := t.setLocked(key, val, false, true)
	if isNew {
		t.size.Add(1)
	}
	return isNew
}

func (t *btree[K, V]) DeleteMutable(key K) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	root := t.root.Load()
	if root == nil {
		return false
	}
	root, removed := t.removeMutable(root, key)
	if removed {
		root = t.shrinkRoot(root)
	}
	t.root.Store(root)
	if removed {
		t.size.Add(-1)
	}
	return removed
}

// removeMutable 与 remove 同算法，但不克隆（树尚未发布给读者）。
func (t *btree[K, V]) removeMutable(n *btNode[K, V], key K) (*btNode[K, V], bool) {
	for {
		i := btLowerBound(n, key)
		if i < n.n && n.keys[i] == key {
			if n.isLeaf() {
				t.removeKeyAt(n, i)
				return n, true
			}
			var merged bool
			n, i, merged = t.ensureChild(n, i, false)
			if merged {
				continue
			}
			// 同 remove：向右兄弟借位可能把待删键下沉进子树，需重新定位
			if j := btLowerBound(n, key); j >= n.n || n.keys[j] != key {
				child, removed := t.removeMutable(n.kids[j], key)
				n.kids[j] = child
				return n, removed
			}
			pk, pv := t.maxKey(n.kids[i])
			n.keys[i], n.vals[i] = pk, pv
			child, _ := t.removeMutable(n.kids[i], pk)
			n.kids[i] = child
			return n, true
		}
		if n.isLeaf() {
			return n, false
		}
		var merged bool
		n, i, merged = t.ensureChild(n, i, false)
		if merged {
			continue
		}
		child, removed := t.removeMutable(n.kids[i], key)
		n.kids[i] = child
		if !removed {
			return n, false
		}
		return n, true
	}
}

// UpdateMutable 仅覆盖已存在的键（不插入），返回是否命中。
func (t *btree[K, V]) UpdateMutable(key K, val V) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.root.Load()
	for n != nil {
		i := btLowerBound(n, key)
		if i < n.n && n.keys[i] == key {
			n.vals[i] = val
			return true
		}
		if n.isLeaf() {
			return false
		}
		n = n.kids[i]
	}
	return false
}

// ---------------- 有序扫描 ----------------

func (t *btree[K, V]) scanAll(visit func(key K, value V)) {
	t.scanNode(t.root.Load(), nil, nil, func(k K, v V) bool {
		visit(k, v)
		return true
	})
}

func (t *btree[K, V]) scanRangeStop(min, max *K, visit func(key K, value V) bool) {
	t.scanNode(t.root.Load(), min, max, visit)
}

func (t *btree[K, V]) scanRangeLimit(min *K, limit int, visit func(key K, value V) bool) bool {
	if limit <= 0 {
		return false
	}
	count := 0
	t.scanNode(t.root.Load(), min, nil, func(k K, v V) bool {
		if !visit(k, v) {
			return false
		}
		count++
		return count < limit
	})
	return count >= limit
}

// scanNode 中序递归遍历；返回 false 表示已被访问者要求提前终止。
// 剪枝与红黑树版本一致：越过 max 立即整体停止，min 之前的分支直接跳过。
func (t *btree[K, V]) scanNode(n *btNode[K, V], min, max *K, visit func(K, V) bool) bool {
	if n == nil {
		return true
	}
	start := 0
	if min != nil {
		start = btLowerBound(n, *min)
	}
	leaf := n.isLeaf()
	for i := start; i < n.n; i++ {
		if !leaf {
			if !t.scanNode(n.kids[i], min, max, visit) {
				return false
			}
		}
		if max != nil && *max < n.keys[i] {
			return false
		}
		if !visit(n.keys[i], n.vals[i]) {
			return false
		}
	}
	if !leaf {
		if max != nil && n.n > 0 && *max < n.keys[n.n-1] {
			return false
		}
		return t.scanNode(n.kids[n.n], min, max, visit)
	}
	return true
}
