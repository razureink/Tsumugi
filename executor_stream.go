package main

// ==================== 简单单表 SELECT 的流式入口 ====================
//
// executor.go 只提供算子骨架；这里把「SQL 文本 → 算子」接起来，让协议层、
// 导出器或测试可以绕开 execSelectPlan 的 [][]interface{} 物化，直接逐行取。
//
// 只接受**简单单表 SELECT**（限制见 planStreamSelect）。其余语句返回
// ErrNotSimpleSelect，调用方应当回落到原来的通用执行器：流式路径在能力
// 范围内与通用路径语义一致，只是一条更省内存的等价路径。
//
// 明确不支持（回落到通用执行器即可）：
//   - JOIN / GROUP BY / HAVING / DISTINCT / ORDER BY / OFFSET；
//   - 投影里出现表达式、函数、聚合；
//   - WHERE 不能完整下推成等值（=）或范围条件（> >= < <= <>）；
//     带限定符的列（t.age = 1）也视为不可下推——extractConds 只接受裸列名；
//   - 虚拟表（information_schema.* / mysql.*）：没有 pkTree，无流式可言。
//
// 顺序约定：本路径按主键升序产出。通用路径在没有二级索引时也是这个顺序；
// 一旦将来用二级索引扫描，顺序可能不同——真正接入协议前需要先决定
// 是否要保留"索引顺序"语义。
//
// 协议接入现状（B1 未接入 server.go 的原因）：
// 原生协议的结果集帧是 "RespRows + 列数 + 列名 + **行数** + 行数据"，
// 行数必须先写；协议里也没有"分块/更多行"帧，CLI 与压测客户端都是一次
// 读完整个 RespRows。也就是说，"流式产出"在现有 wire format 下无处可去：
// 数据仍会先堆进一个完整响应（除非新增帧类型和客户端能力协商）。
// 所以这里只把算子与 PlanStreamSelect 准备好：等协议的 chunk 帧落地后，
// 先写列名 → Run 逐行写 → 最后补分块结束标记，就能真正端到端流式。
// 在那之前强行接入只会把内存从 [][]interface{} 挪到 bytes.Buffer，
// 峰值依旧是 O(结果大小)，没有实际收益。

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotSimpleSelect 表示语句不在流式路径的能力范围内。
//
// 判定用 errors.Is。调用方收到它应当回落到通用执行器，而不要把它当成
// 查询错误返回给用户——它只表示"这条语句走不了快路"。
var ErrNotSimpleSelect = errors.New("not a simple single-table select")

// streamSelectPlan 流式 SELECT 的解析结果。
type streamSelectPlan struct {
	table    *Table
	projCols []string // 传给算子的源列名
	outNames []string // 返回给调用方的列头（别名/原始大小写）
	conds    map[string]interface{}
	filters  []RangeCond
	limit    int
}

// PlanStreamSelect 解析语句并返回列头与算子，但**不执行**。
//
// 之所以把"解析"和"执行"分开：响应头（列名）必须在第一行数据之前写出去，
// 而行数往往要扫完才知道。调用方可以先写列名，再 Run 逐行写数据——这正是
// 真正分块输出所需要的顺序（现有协议要求行数前置，见报告中的接入说明）。
//
// 授权：本方法拿不到账号，因此**不做**表级权限校验。将来接到协议层时，
// 必须沿用原路径的 PermSelect 检查，否则会绕过权限。
func (db *DB) PlanStreamSelect(query string) (columns []string, op Operator, err error) {
	pl, err := db.planStreamSelect(query)
	if err != nil {
		return nil, nil, err
	}
	return pl.outNames, newStreamScanOp(pl.table, pl.projCols, pl.conds, pl.filters, nil, nil, pl.limit), nil
}

// StreamSelectSQL 解析并执行简单单表 SELECT，逐行回调 sink，
// 返回列头与实际产出行数。列头在扫描开始前就可确定，因此这里先把 columns
// 填好再 Run，即使扫描中途 sink 报错，调用方仍能拿到列信息。
func (db *DB) StreamSelectSQL(query string, sink rowSink) (columns []string, n int64, err error) {
	if sink == nil {
		return nil, 0, fmt.Errorf("StreamSelectSQL: nil sink")
	}
	cols, op, err := db.PlanStreamSelect(query)
	if err != nil {
		return nil, 0, err
	}
	err = op.Run(func(row []interface{}) error {
		n++
		return sink(row)
	})
	return cols, n, err
}

// planStreamSelect 把 SQL 解析成 streamSelectPlan；不支持时返回 ErrNotSimpleSelect。
func (db *DB) planStreamSelect(query string) (*streamSelectPlan, error) {
	toks, err := tokenizeSQL(query)
	if err != nil {
		return nil, err
	}
	defer putSQLTokens(toks)

	p := &sqlParser{toks: toks}
	if !p.matchKeyword("SELECT") {
		return nil, ErrNotSimpleSelect
	}
	pl, perr := db.parseSelectPlan(p)
	if perr != nil {
		// 语法错误由通用执行器负责报（报错文本保持一致）；这里只表达
		// "我处理不了"，并保留原始错误便于直接调用方排查。
		return nil, fmt.Errorf("%w: %v", ErrNotSimpleSelect, perr)
	}
	if pl.distinct || len(pl.tables) != 1 || len(pl.groupBy) > 0 ||
		pl.having != nil || len(pl.orderBy) > 0 || pl.offset != 0 {
		return nil, ErrNotSimpleSelect
	}

	ft := pl.tables[0]
	lower := strings.ToLower(ft.name)
	if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") {
		return nil, ErrNotSimpleSelect // 虚拟表没有 pkTree
	}
	table := db.getTable(db.qualifyTableIn(ft.name, p.curSchema(db)))
	if table == nil {
		// 缺表是真错误（通用路径也会报），不是"走不了快路"。
		return nil, fmt.Errorf("table not found: %s", ft.name)
	}
	meta := table.Meta()

	// 限定符必须指向本表；单表场景下允许 "" / 表名 / 别名。
	qualOK := func(q string) bool {
		return q == "" || strings.EqualFold(q, ft.alias) || strings.EqualFold(q, ft.name)
	}

	projCols := make([]string, 0, len(pl.items)+len(meta.Fields))
	outNames := make([]string, 0, len(pl.items)+len(meta.Fields))
	for _, it := range pl.items {
		if it.star {
			if !qualOK(it.qual) {
				return nil, ErrNotSimpleSelect
			}
			for i := range meta.Fields {
				projCols = append(projCols, meta.Fields[i].Name)
				outNames = append(outNames, meta.Fields[i].Name)
			}
			continue
		}
		if it.e == nil || it.e.kind != exCol || !qualOK(it.e.table) {
			return nil, ErrNotSimpleSelect
		}
		if meta.fieldIndexOf(it.e.col) < 0 {
			return nil, fmt.Errorf("unknown column '%s'", it.e.col)
		}
		projCols = append(projCols, it.e.col)
		outNames = append(outNames, it.name)
	}
	if len(projCols) == 0 {
		return nil, ErrNotSimpleSelect
	}

	// WHERE 必须能完全下推；剩下任何残余表达式都说明需要逐行求值能力，
	// 交给通用执行器（它用 map 行 + expression eval）。
	conds, filters, rest := extractConds(pl.where)
	if rest != nil {
		return nil, ErrNotSimpleSelect
	}
	// extractConds 不做列校验，这里补上，避免"未知列被当成恒假条件"
	// 静默返回空集（通用路径会报 unknown column）。
	for name := range conds {
		if meta.fieldIndexOf(name) < 0 {
			return nil, fmt.Errorf("unknown column '%s'", name)
		}
	}
	for i := range filters {
		if meta.fieldIndexOf(filters[i].Field) < 0 {
			return nil, fmt.Errorf("unknown column '%s'", filters[i].Field)
		}
	}

	return &streamSelectPlan{
		table:    table,
		projCols: projCols,
		outNames: outNames,
		conds:    conds,
		filters:  filters,
		limit:    pl.limit,
	}, nil
}
