package main

// ==================== 索引树门面（可切换实现） ====================
// 主键索引与二级索引都用同一套有序映射语义，但"用哪种平衡结构"是可以切换的：
//
//	llrb  —— 左倾红黑树（默认，成熟稳定）
//	btree —— 写时复制 B 树（分支因子 8，写路径克隆层数从 ~22 降到 ~7）
//
// 两者共用完全相同的对外 API 与并发模型（COW + 原子换根，读者无锁），
// 因此调用方（Table.pkTree、idxTrees）完全不需要感知当前实现。
//
// 切换方式：配置文件 tree_kind / 环境变量 TSUMUGI_TREE / 后台设置页。
// 生效时机：下一次建树时（即重启后从 WAL 重放建索引，或新建表）——
// 树上已经存了数据，无法原地换结构，必须重建。

import (
	"cmp"
	"runtime"
	"sync"
	"sync/atomic"
)

// treeBackend 是索引树的内部实现接口。两种结构都必须完整实现它，
// 语义（排序、范围扫描剪枝、SetNX/Delete 返回值、Mutable 变体）必须一致。
type treeBackend[K cmp.Ordered, V any] interface {
	Size() int64
	Get(key K) (V, bool)
	Set(key K, val V) bool
	SetNX(key K, val V) bool
	Delete(key K) bool
	scanAll(visit func(key K, value V))
	scanRangeStop(min, max *K, visit func(key K, value V) bool)
	scanRangeLimit(min *K, limit int, visit func(key K, value V) bool) bool
	// 以下为 WAL 重放专用：树尚未发布给并发读者时原地修改，避免写时复制。
	SetMutable(key K, val V) bool
	SetNXMutable(key K, val V) bool
	DeleteMutable(key K) bool
	UpdateMutable(key K, val V) bool
}

// treeKind 索引树实现的选择。
type treeKind int32

const (
	treeKindLLRB  treeKind = iota // 左倾红黑树（默认）
	treeKindBTree                 // 写时复制 B 树
)

// treeMode 并发/写路径策略，与"用哪种结构"正交：
//
//	treeModeCOW  —— 写时复制：写入克隆路径后原子换根，读者完全无锁（默认）
//	treeModeLock —— 读写锁 + 原地更新：写入不再克隆，快一个数量级；
//	                读者需持读锁，长扫描期间写入会被阻塞，且不再有"快照隔离"。
type treeMode int32

const (
	treeModeCOW  treeMode = iota // 写时复制（默认，无锁读）
	treeModeLock                 // 读写锁 + 原地更新（快写，读加锁）
	treeModeAuto                 // 自适应：单线程用原地更新，检测到并发即单向升级为 COW
)

func (m treeMode) String() string {
	switch m {
	case treeModeLock:
		return "lock"
	case treeModeAuto:
		return "auto"
	}
	return "cow"
}

// parseTreeMode 解析配置里的并发模式名。
func parseTreeMode(s string) treeMode {
	switch s {
	case "lock", "rw", "rwlock", "mutable":
		return treeModeLock
	case "auto", "adaptive":
		return treeModeAuto
	}
	return treeModeCOW
}

// activeTreeKind / activeTreeMode 当前生效的选择。用原子变量是为了让后台切换
// 与建树并发安全：建树时读一次快照，之后这棵树就固定用它。
var activeTreeKind atomic.Int32
var activeTreeMode atomic.Int32

// setTreeKind 设定新树使用的实现（对已存在的树无效——它们无法原地换结构）。
func setTreeKind(k treeKind) { activeTreeKind.Store(int32(k)) }

// currentTreeKind 返回当前选定的实现。
func currentTreeKind() treeKind { return treeKind(activeTreeKind.Load()) }

// setTreeMode 设定新树使用的并发模式。
func setTreeMode(m treeMode) { activeTreeMode.Store(int32(m)) }

// currentTreeMode 返回当前选定的并发模式。
func currentTreeMode() treeMode { return treeMode(activeTreeMode.Load()) }

// String 便于日志与设置页展示。
func (k treeKind) String() string {
	if k == treeKindBTree {
		return "btree"
	}
	return "llrb"
}

// parseTreeKind 解析配置/环境变量里的名字；无法识别时返回默认实现。
func parseTreeKind(s string) treeKind {
	switch s {
	case "btree", "b-tree", "b", "B":
		return treeKindBTree
	case "llrb", "rb", "rbtree", "rb-tree":
		return treeKindLLRB
	}
	return treeKindLLRB
}

// RBTree 是有序映射门面：所有方法委托给当前选定的实现。
// 保留了原来的类型名与全部方法签名，因此 db.go / sql_*.go 一行都不用改。
type RBTree[K cmp.Ordered, V any] struct {
	impl treeBackend[K, V]
	// lockMode 为 true 时用读写锁保护、写路径走原地更新（不再写时复制）。
	// 建树时按当时的全局模式固定下来：同一棵树不能中途换并发策略，
	// 否则已持有快照的读者会看到被原地修改的节点。
	lockMode bool
	// adaptive 自适应模式：单线程时走原地更新（快），一旦发现有并发操作，
	// 单向升级为 COW（无锁读）并永久保持。
	adaptive bool
	rw       sync.RWMutex

	// ---- 自适应模式的并发探测 ----
	// inflight 当前正在操作本树的 goroutine 数；只有"独占"时才允许原地修改。
	inflight  atomic.Int32
	upgrading atomic.Bool // 升级进行中：新操作在门口等待
	upgraded  atomic.Bool // 已升级为 COW（单向，不再回落）
	upOnce    sync.Once
}

// treeAccess 一次操作需要哪种访问策略。
type treeAccess struct {
	writeMutable bool // 写路径是否原地修改（免写时复制）
	lockRead     bool // 读路径是否需要持读锁
	counted      bool // 是否占用了 inflight 名额（退出时要归还）
}

var accessCOW = treeAccess{}

// enter 决定本次操作使用的策略（自适应模式在这里做并发探测与升级）。
func (t *RBTree[K, V]) enter() treeAccess {
	if !t.adaptive {
		// 固定模式：lock 走读写锁 + 原地更新；cow 走无锁读 + 写时复制
		if t.lockMode {
			return treeAccess{writeMutable: true, lockRead: true}
		}
		return accessCOW
	}
	if t.upgraded.Load() {
		return accessCOW // 已升级：之后永远是 COW，零额外开销
	}
	for {
		// 升级进行中：在门口等待，保证升级者能把在途操作排空
		if t.upgrading.Load() {
			runtime.Gosched()
			continue
		}
		t.inflight.Add(1)
		if t.upgrading.Load() {
			// 与升级者竞争失败：归还名额后重试
			t.inflight.Add(-1)
			continue
		}
		if t.inflight.Load() > 1 {
			// 检测到并发：放弃原地修改，触发单向升级
			t.inflight.Add(-1)
			t.upgrade()
			continue
		}
		// 成功独占：可以安全地原地修改，读也不需要加锁
		return treeAccess{writeMutable: true, counted: true}
	}
}

// exit 归还 inflight 名额（仅自适应模式且成功独占时需要）。
func (t *RBTree[K, V]) exit(a treeAccess) {
	if a.counted {
		t.inflight.Add(-1)
	}
}

// upgrade 把本树单向升级为 COW。只在 inflight 归零后设置 upgraded，
// 保证升级完成的那一刻没有任何原地写者仍在改节点。
func (t *RBTree[K, V]) upgrade() {
	t.upOnce.Do(func() {
		t.upgrading.Store(true)
		for t.inflight.Load() != 0 {
			runtime.Gosched()
		}
		t.upgraded.Store(true)
		t.upgrading.Store(false)
	})
}

// NewRBTree 按当前选择构造一棵树（键须为可排序类型）。
func NewRBTree[K cmp.Ordered, V any]() *RBTree[K, V] {
	mode := currentTreeMode()
	lock := mode == treeModeLock
	adaptive := mode == treeModeAuto
	if currentTreeKind() == treeKindBTree {
		return &RBTree[K, V]{impl: newBTree[K, V](), lockMode: lock, adaptive: adaptive}
	}
	return &RBTree[K, V]{impl: newLLRB[K, V](), lockMode: lock, adaptive: adaptive}
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

// Kind 返回这棵树实际使用的实现（建树时确定，之后不再变化）。
func (t *RBTree[K, V]) Kind() treeKind {
	switch t.impl.(type) {
	case *btree[K, V]:
		return treeKindBTree
	default:
		return treeKindLLRB
	}
}

// Mode 返回这棵树实际使用的并发模式。
func (t *RBTree[K, V]) Mode() treeMode {
	if t.adaptive {
		if t.upgraded.Load() {
			return treeModeCOW // 已自升级为 COW
		}
		return treeModeAuto
	}
	if t.lockMode {
		return treeModeLock
	}
	return treeModeCOW
}

// Upgraded 报告自适应模式是否已经检测到并发并升级为 COW。
func (t *RBTree[K, V]) Upgraded() bool { return t.upgraded.Load() }

// ---- 读路径：COW 完全无锁；lock 模式持读锁；auto 独占时也无需加锁 ----

func (t *RBTree[K, V]) Size() int64 { return t.impl.Size() }

func (t *RBTree[K, V]) Get(key K) (V, bool) {
	a := t.enter()
	defer t.exit(a)
	if a.lockRead {
		t.rw.RLock()
		defer t.rw.RUnlock()
	}
	return t.impl.Get(key)
}

func (t *RBTree[K, V]) scanAll(visit func(K, V)) {
	a := t.enter()
	defer t.exit(a)
	if a.lockRead {
		t.rw.RLock()
		defer t.rw.RUnlock()
	}
	t.impl.scanAll(visit)
}

func (t *RBTree[K, V]) scanRangeStop(min, max *K, visit func(K, V) bool) {
	a := t.enter()
	defer t.exit(a)
	if a.lockRead {
		t.rw.RLock()
		defer t.rw.RUnlock()
	}
	t.impl.scanRangeStop(min, max, visit)
}

func (t *RBTree[K, V]) scanRangeLimit(min *K, limit int, visit func(K, V) bool) bool {
	a := t.enter()
	defer t.exit(a)
	if a.lockRead {
		t.rw.RLock()
		defer t.rw.RUnlock()
	}
	return t.impl.scanRangeLimit(min, limit, visit)
}

// ---- 写路径：原地更新模式下持写锁（固定 lock 模式）或独占（自适应模式） ----

func (t *RBTree[K, V]) Set(key K, val V) bool {
	a := t.enter()
	defer t.exit(a)
	if a.writeMutable {
		if a.lockRead {
			t.rw.Lock()
			defer t.rw.Unlock()
		}
		return t.impl.SetMutable(key, val)
	}
	return t.impl.Set(key, val)
}

func (t *RBTree[K, V]) SetNX(key K, val V) bool {
	a := t.enter()
	defer t.exit(a)
	if a.writeMutable {
		if a.lockRead {
			t.rw.Lock()
			defer t.rw.Unlock()
		}
		return t.impl.SetNXMutable(key, val)
	}
	return t.impl.SetNX(key, val)
}

func (t *RBTree[K, V]) Delete(key K) bool {
	a := t.enter()
	defer t.exit(a)
	if a.writeMutable {
		if a.lockRead {
			t.rw.Lock()
			defer t.rw.Unlock()
		}
		return t.impl.DeleteMutable(key)
	}
	return t.impl.Delete(key)
}

// ---- 重放专用：调用方已保证独占，不再加锁 ----

func (t *RBTree[K, V]) SetMutable(key K, val V) bool { return t.impl.SetMutable(key, val) }
func (t *RBTree[K, V]) SetNXMutable(key K, val V) bool {
	return t.impl.SetNXMutable(key, val)
}
func (t *RBTree[K, V]) DeleteMutable(key K) bool        { return t.impl.DeleteMutable(key) }
func (t *RBTree[K, V]) UpdateMutable(key K, val V) bool { return t.impl.UpdateMutable(key, val) }
