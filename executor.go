package main

// ==================== 流式执行算子（B1 第一刀） ====================
//
// 现状：所有 SELECT 最终都物化成 [][]interface{}（selectRowsProjected /
// selectRBytes 都是"扫完再返回"），协议层拿到完整结果集后才整体编码。
// 于是峰值内存 = O(结果行数 × 行宽)，与"边算边发"天然冲突：大表导出、
// 逐行聚合、真正的分块输出都只能靠 LIMIT 硬扛或把整表读进内存。
//
// 这里的第一刀只改最底层的一段管线：
//
//	pkTree.scanRangeStop → 字节级谓词 → 窄投影 → sink(row)
//
// 改成算子后，Run 逐行回调，回调返回后该行立即被复用，峰值内存只与
// **单行宽度**有关，与扫描行数、命中行数都无关。
//
// 刻意复用而不是重写：
//   - 行解码用 db.go 的 decodeRowProjected（跳过未选列，不建 map）；
//   - 谓词求值用 db.go 的 scanConds（在行字节上比较，零分配）。
// 本文件不修改 db.go / sql_expr.go / sql_parser.go / wal_v3.go。

import (
	"fmt"
	"time"
)

// rowSink 接收算子产出的一行。
//
// 生命周期约定：row 只在本次回调内有效——下一次回调会覆盖同一块缓冲，
// 需要保留的行必须自行 copy。行内的 string 直接引用树内行字节
// （unsafeString），在该行被 UPDATE/DELETE 之前一直有效；这与 handleSelect
// 把树内原始字节直接写给客户端的零拷贝约定一致。
//
// 返回非 nil 错误会立即中止扫描，错误原样从 Run 返回。
type rowSink func(row []interface{}) error

// Operator 流式查询执行体：Run 不返回结果集，而是把每一行推给 sink。
//
// 同一个 Operator 可被多次 Run（每次都是全新扫描）。实现约定配置不可变、
// Run 的全部状态都是局部变量，因此并发 Run 是安全的。
type Operator interface {
	Run(sink rowSink) error
}

// streamExpireBatch 单次扫描内最多登记多少条过期主键。
//
// 回调持树读锁，不能就地 Delete，只能收集后锁外清理；但"收集"本身也会
// 无界增长——过期行很多时会把流式的 O(1) 内存又变成 O(过期行数)。
// 超过上限就放弃本轮回收（过期行仍会被逐行过滤，不影响结果正确性），
// 由 Table.CleanExpired 的定期任务兜底。
const streamExpireBatch = 1024

// streamProjection 输出列到源字段的映射，直接喂给 decodeRowProjected。
// fieldToOut == nil 表示恒等投影（SELECT *）。
type streamProjection struct {
	fieldToOut []int // 长度 = len(meta.Fields)，值 = 输出下标，-1 表示不输出
	width      int
	// 同一列在投影里出现多次时的补充拷贝（SELECT id, id）：
	// decodeRowProjected 一个源字段只能填一个输出位置。
	copyFrom []int
	copyTo   []int
}

// buildStreamProjection 把输出列名解析成字段下标映射。
// cols 为空表示 SELECT *（恒等投影，按 meta.Fields 顺序输出）。
func buildStreamProjection(meta *TableMeta, cols []string) (*streamProjection, error) {
	if len(cols) == 0 {
		return &streamProjection{width: len(meta.Fields)}, nil
	}
	fieldToOut := make([]int, len(meta.Fields))
	for i := range fieldToOut {
		fieldToOut[i] = -1
	}
	p := &streamProjection{fieldToOut: fieldToOut, width: len(cols)}
	for out, name := range cols {
		fi := meta.fieldIndexOf(name)
		if fi < 0 {
			return nil, fmt.Errorf("unknown column '%s'", name)
		}
		if fieldToOut[fi] >= 0 {
			p.copyFrom = append(p.copyFrom, fieldToOut[fi])
			p.copyTo = append(p.copyTo, out)
			continue
		}
		fieldToOut[fi] = out
	}
	return p, nil
}

// streamScanOp 单表扫描算子：pkTree 范围扫描 + 字节级谓词 + 窄投影。
//
// 字段全部只读（构造后不再修改），Run 内不写任何共享状态——所以既能被
// 多次 Run，也能被多个 goroutine 同时 Run。
type streamScanOp struct {
	t       *Table
	cols    []string // 输出列名；空 = SELECT *
	conds   map[string]interface{}
	filters []RangeCond
	// minKey/maxKey 是显式主键扫描区间（可被上层的主键范围条件下推）。
	minKey, maxKey *int64
	limit          int // <0 表示不限制
}

// newStreamScanOp 构造算子。列名校验推迟到 Run（meta 可能因 ALTER 变化，
// 每次 Run 都按当时的 meta 解析，避免持有过期快照）。
func newStreamScanOp(t *Table, cols []string, conds map[string]interface{}, filters []RangeCond, minKey, maxKey *int64, limit int) *streamScanOp {
	return &streamScanOp{
		t:       t,
		cols:    cols,
		conds:   conds,
		filters: filters,
		minKey:  minKey,
		maxKey:  maxKey,
		limit:   limit,
	}
}

// streamScan 是一次性调用的便捷入口：构造算子并立即 Run。
// 与 newStreamScanOp(...).Run(sink) 等价，测试和未来的协议层接入都直接用它。
func streamScan(t *Table, cols []string, conds map[string]interface{}, filters []RangeCond, minKey, maxKey *int64, limit int, sink rowSink) error {
	return newStreamScanOp(t, cols, conds, filters, minKey, maxKey, limit).Run(sink)
}

// Run 实现 Operator。
//
// 只保留一块行缓冲：每命中一行就解码进同一块 []interface{} 并回调，
// 不 append 到任何结果切片——这是"峰值内存与结果行数无关"的全部秘密。
func (op *streamScanOp) Run(sink rowSink) error {
	if op.t == nil {
		return fmt.Errorf("streamScan: nil table")
	}
	if sink == nil {
		return fmt.Errorf("streamScan: nil sink")
	}
	// LIMIT 0 直接空结果：也避免下面为一行数据做无谓分配。
	if op.limit == 0 {
		return nil
	}
	t := op.t
	meta := t.Meta()
	proj, err := buildStreamProjection(meta, op.cols)
	if err != nil {
		return err
	}
	scan := newScanConds(meta, op.conds, op.filters)

	// 单块行缓冲，回调之间复用。sink 想保留必须自己 copy（见 rowSink 注释）。
	row := make([]interface{}, proj.width)
	send := func(data []byte) error {
		decodeRowProjected(meta, data, row, proj.fieldToOut)
		for i := range proj.copyTo {
			row[proj.copyTo[i]] = row[proj.copyFrom[i]]
		}
		return sink(row)
	}

	// 主键点查优先：与 selectRBytes 的三级分发保持一致，省掉整树扫描。
	// 只认 int64（与 selectRBytes 相同），其它类型自然退化为下面的范围扫描。
	if v, ok := op.conds[meta.PK]; ok {
		if pk, isInt := v.(int64); isInt {
			data, found := t.pkTree.Get(pk)
			if !found {
				return nil
			}
			if e := decodeExpireAt(data); e > 0 && time.Now().UnixNano() > e {
				t.pkTree.Delete(pk)
				return nil
			}
			if !scan.matchRow(meta, data) {
				return nil
			}
			return send(data)
		}
	}

	var expired []int64
	now := time.Now().UnixNano()
	emitted := 0
	var sinkErr error
	t.pkTree.scanRangeStop(op.minKey, op.maxKey, func(key int64, data []byte) bool {
		// TTL：过期行不产出；先登记，扫描结束（读锁释放）后再删。
		if e := decodeExpireAt(data); e > 0 && now > e {
			if len(expired) < streamExpireBatch {
				expired = append(expired, key)
			}
			return true
		}
		if !scan.matchRow(meta, data) {
			return true
		}
		if err := send(data); err != nil {
			sinkErr = err
			return false // 下游说停就停，不再浪费时间扫描
		}
		emitted++
		return op.limit < 0 || emitted < op.limit
	})
	// 回调持读锁，不能就地 Delete；锁外统一清理。
	for _, k := range expired {
		t.pkTree.Delete(k)
	}
	return sinkErr
}
