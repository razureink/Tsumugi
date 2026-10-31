package main

// ==================== SELECT 通用执行路径 ====================
// 服务"快路径覆盖不到"的查询：表达式投影、JOIN、GROUP BY/HAVING、
// DISTINCT、UNION，以及带函数/IN 的 WHERE。
//
// 快路径（parseSelect 里的字节级谓词 + pkTree 点查）保持原样，
// 只有检测到上述特性时才走这里，因此简单查询的性能不受影响。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// selItem SELECT 列表的一项。
type selItem struct {
	e    *expr
	name string // 输出列名（别名或推导名）
	star bool   // *
	qual string // tbl.* 的限定符
}

// fromTab FROM/JOIN 中的一个表。
type fromTab struct {
	name  string // 物理表名（可能带 db. 前缀）
	alias string // 别名/表名
}

// selectPlan 解析后的查询计划。
type selectPlan struct {
	items     []selItem
	distinct  bool
	tables    []fromTab
	on        []*expr // 与 tables[i] (i>=1) 对应的 ON 条件
	where     *expr
	groupBy   []*expr
	having    *expr
	orderBy   []*expr
	joinTypes []string // 每张表的连接类型（""=首表/逗号、INNER、LEFT、RIGHT、CROSS）
	desc      []bool
	limit     int
	offset    int64
}

// displayName 推导输出列名。
func (e *expr) displayName() string {
	switch e.kind {
	case exCol:
		if e.raw != "" {
			return e.raw
		}
		return e.col
	case exStar:
		return "*"
	case exConst:
		return toStr(e.val)
	case exAgg, exFunc:
		parts := make([]string, 0, len(e.args))
		for _, a := range e.args {
			parts = append(parts, a.displayName())
		}
		if e.kind == exAgg && e.dist {
			return e.fn + "(DISTINCT " + strings.Join(parts, ",") + ")"
		}
		return e.fn + "(" + strings.Join(parts, ",") + ")"
	case exCase:
		return "case"
	}
	return "expr"
}

// parseSelectGeneral 解析并执行通用 SELECT（p 已消费 SELECT 关键字）。
func (db *DB) parseSelectGeneral(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	plan, e := db.parseSelectPlan(p)
	if e != nil {
		err = e
		return
	}
	// 单表通用路径（DISTINCT / GROUP BY / 表达式投影）同样用 Go 值比较条件，
	// 需要把日期/十进制常量折算成存储值。多表 JOIN 的列归属无法在这里可靠
	// 判定，保持原样（见最终报告的降级说明）。
	if len(plan.tables) == 1 {
		if t := db.getTable(plan.tables[0].name); t != nil {
			coerceExprConsts(plan.where, t.Meta())
		}
	}
	columns, rows, err = db.execSelectPlan(p, plan)
	if err != nil {
		return
	}
	// UNION [ALL] 由 runSQLTokensAs 在 token 层拆分（findTopLevelUnion），此处不再处理
	return
}

// parseSelectPlan 解析 SELECT ... [LIMIT ...]（不含 UNION 部分；p 已消费 SELECT）。
func (db *DB) parseSelectPlan(p *sqlParser) (*selectPlan, error) {
	pl := &selectPlan{limit: -1}
	if p.peek().is("DISTINCT") {
		p.next()
		pl.distinct = true
	} else {
		p.matchKeyword("ALL")
	}
	for {
		if p.peek().kind == kindSym && p.peek().val == "*" {
			p.next()
			pl.items = append(pl.items, selItem{star: true})
		} else if (p.peek().kind == kindIdent || p.peek().kind == kindKw) &&
			p.toks[p.i+1].kind == kindSym && p.toks[p.i+1].val == "." &&
			p.toks[p.i+2].kind == kindSym && p.toks[p.i+2].val == "*" {
			pl.items = append(pl.items, selItem{star: true, qual: p.next().val})
			p.next() // .
			p.next() // *
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			name := ""
			if p.matchKeyword("AS") {
				if t := p.next(); t.kind == kindIdent || t.kind == kindKw || t.kind == kindStr {
					name = t.val
				} else {
					return nil, fmt.Errorf("expected alias after AS")
				}
			} else if t := p.peek(); (t.kind == kindIdent || t.kind == kindStr) && !isReservedAliasWord(t.val) {
				name = t.val
				p.next()
			}
			if name == "" {
				name = e.displayName()
			}
			pl.items = append(pl.items, selItem{e: e, name: name})
		}
		if !p.matchSym(",") {
			break
		}
	}
	if !p.matchKeyword("FROM") {
		return pl, nil
	}
	if p.peek().is("DUAL") {
		p.next()
		return pl, nil
	}
	// 首个表
	{
		nameTok := p.next()
		if nameTok.kind != kindIdent && nameTok.kind != kindKw && nameTok.kind != kindStr {
			return nil, fmt.Errorf("expected table name, got %q", nameTok.val)
		}
		tblName := nameTok.val
		// 限定名 db.table
		for p.peek().kind == kindSym && p.peek().val == "." {
			p.next()
			part := p.next()
			if part.kind != kindIdent && part.kind != kindKw && part.kind != kindStr {
				return nil, fmt.Errorf("expected table name after %s.", tblName)
			}
			tblName += "." + part.val
		}
		tab := fromTab{name: tblName, alias: tblName}
		if p.matchKeyword("AS") {
			if t := p.next(); t.kind == kindIdent || t.kind == kindKw {
				tab.alias = t.val
			}
		} else if t := p.peek(); t.kind == kindIdent && !isReservedAliasWord(t.val) {
			tab.alias = t.val
			p.next()
		}
		pl.tables = append(pl.tables, tab)
		// pl.on / pl.joinTypes 与 pl.tables 一一对齐：首表无连接条件
		pl.on = append(pl.on, nil)
		pl.joinTypes = append(pl.joinTypes, "")
	}
	// 后续 JOIN / 逗号连接（parseJoinTail 自己消费被连接的表）
	for p.parseJoinTail(p, pl) {
	}
	if p.matchKeyword("WHERE") {
		we, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		pl.where = we
	}
	if p.matchKeyword("GROUP") {
		if !p.matchKeyword("BY") {
			return nil, fmt.Errorf("expected BY after GROUP")
		}
		for {
			ge, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			pl.groupBy = append(pl.groupBy, ge)
			if !p.matchSym(",") {
				break
			}
		}
		p.matchKeyword("WITH")
		p.matchKeyword("ROLLUP")
	}
	if p.matchKeyword("HAVING") {
		he, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		pl.having = he
	}
	if p.matchKeyword("ORDER") {
		if !p.matchKeyword("BY") {
			return nil, fmt.Errorf("expected BY after ORDER")
		}
		for {
			oe, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			desc := false
			if p.matchKeyword("DESC") {
				desc = true
			} else {
				p.matchKeyword("ASC")
			}
			pl.orderBy = append(pl.orderBy, oe)
			pl.desc = append(pl.desc, desc)
			if !p.matchSym(",") {
				break
			}
		}
	}
	if p.matchKeyword("LIMIT") {
		n1, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		f1, _ := toFloat(n1)
		if p.matchSym(",") {
			n2, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f2, _ := toFloat(n2)
			pl.offset = int64(f1)
			pl.limit = int(f2)
		} else if p.matchKeyword("OFFSET") {
			n2, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f2, _ := toFloat(n2)
			pl.offset = int64(f2)
			pl.limit = int(f1)
		} else {
			pl.limit = int(f1)
		}
	}
	if e := p.expectEnd(); e != nil {
		return nil, e
	}
	return pl, nil
}

// parseJoinTail 解析 JOIN 子句（含逗号连接）；返回 true 表示又连接了一张表。
// 该函数自己消费被连接的表名与 ON/USING 条件，调用方只需循环调用。
func (p *sqlParser) parseJoinTail(p2 *sqlParser, pl *selectPlan) bool {
	commaJoin := p.matchSym(",")
	joinType := ""
	if !commaJoin {
		switch {
		case p.peek().is("JOIN"):
			p.next()
			joinType = "INNER"
		case p.peek().is("INNER"):
			p.next()
			p.matchKeyword("OUTER")
			if !p.matchKeyword("JOIN") {
				return false
			}
			joinType = "INNER"
		case p.peek().is("CROSS"):
			p.next()
			if !p.matchKeyword("JOIN") {
				return false
			}
			joinType = "CROSS"
		case p.peek().is("LEFT"):
			p.next()
			p.matchKeyword("OUTER")
			if !p.matchKeyword("JOIN") {
				return false
			}
			joinType = "LEFT"
		case p.peek().is("RIGHT"):
			p.next()
			p.matchKeyword("OUTER")
			if !p.matchKeyword("JOIN") {
				return false
			}
			joinType = "RIGHT"
		default:
			return false
		}
	}
	nt := p.next()
	if nt.kind != kindIdent && nt.kind != kindKw && nt.kind != kindStr {
		return false
	}
	ntab := fromTab{name: nt.val, alias: nt.val}
	if p.matchKeyword("AS") {
		if t := p.next(); t.kind == kindIdent || t.kind == kindKw {
			ntab.alias = t.val
		}
	} else if t := p.peek(); t.kind == kindIdent && !isReservedAliasWord(t.val) {
		ntab.alias = t.val
		p.next()
	}
	// 右连接交换两侧（数据量小，嵌套循环可接受）
	if joinType == "RIGHT" {
		pl.tables = append(pl.tables, ntab)
		i := len(pl.tables) - 1
		pl.tables[i-1], pl.tables[i] = pl.tables[i], pl.tables[i-1]
		joinType = "LEFT" // 交换两侧后等价于左连接（on/joinTypes 由函数尾部追加）
	} else {
		pl.tables = append(pl.tables, ntab)
	}
	var onExpr *expr
	if p.matchKeyword("ON") {
		oe, err := p.parseExpr()
		if err != nil {
			return false
		}
		onExpr = oe
	} else if p.matchKeyword("USING") {
		if !p.matchSym("(") {
			return false
		}
		var cols []string
		for {
			t := p.next()
			if t.kind == kindIdent || t.kind == kindKw {
				cols = append(cols, t.val)
			}
			if !p.matchSym(",") {
				break
			}
		}
		if !p.matchSym(")") {
			return false
		}
		left := pl.tables[len(pl.tables)-2].alias
		right := pl.tables[len(pl.tables)-1].alias
		for _, c := range cols {
			eq := &expr{kind: exFunc, fn: "=", args: []*expr{
				{kind: exCol, table: left, col: c},
				{kind: exCol, table: right, col: c},
			}}
			if onExpr == nil {
				onExpr = eq
			} else {
				onExpr = &expr{kind: exFunc, fn: "AND", args: []*expr{onExpr, eq}}
			}
		}
	}
	pl.on = append(pl.on, onExpr)
	pl.joinTypes = append(pl.joinTypes, joinType)
	return true
}

// isReservedAliasWord 判断标识符是否是不可当作别名的保留字。
func isReservedAliasWord(s string) bool {
	switch strings.ToUpper(s) {
	case "FROM", "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "JOIN", "INNER",
		"LEFT", "RIGHT", "CROSS", "ON", "USING", "UNION", "AND", "OR", "AS", "BY",
		"ASC", "DESC", "IN", "IS", "LIKE", "BETWEEN", "NOT", "SELECT", "DISTINCT", "ALL":
		return true
	}
	return false
}

// typeTag 值的类型标签（DISTINCT/UNION 去重时需要区分 1 与 "1"）。
func typeTag(v interface{}) string {
	switch v.(type) {
	case nil:
		return "n"
	case string:
		return "s"
	case bool:
		return "b"
	default:
		return "i"
	}
}

// selectNeedsGeneral 判断当前 SELECT 是否需要走通用路径：
// 含 JOIN / GROUP BY / HAVING / DISTINCT / UNION，或投影列表里出现
// 函数调用与运算符（说明不是"裸列 + *"的简单投影），或 FROM 之后出现
// 括号（IN 列表 / WHERE 里的函数 / 子查询）。
func (p *sqlParser) selectNeedsGeneral() bool {
	// 单趟扫描判定（此前是 4~5 趟，SQL 点查因此多出 300ns+）：
	// 顶层括号 / 投影运算符 / JOIN / GROUP / UNION / DISTINCT / 多表逗号 /
	// 投影里的 IS / NOT LIKE 一旦命中即转通用路径。
	depth := 0
	seenFrom := false
	hasOrder := false
	orderIdx := -1
	for i := p.i; i < len(p.toks); i++ {
		t := p.toks[i]
		if t.kind == kindSym {
			switch t.val {
			case "(":
				if depth == 0 {
					return true // 顶层函数调用 / IN 列表 / 子查询
				}
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case ",":
				if depth == 0 && seenFrom {
					return true // FROM 之后的多表列表
				}
			case "*", ".":
			default:
				if depth == 0 && !seenFrom {
					return true // 投影里的顶层运算符
				}
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if t.kind == kindKw {
			switch t.upper {
			case "GROUP", "HAVING", "UNION", "DISTINCT":
				return true
			case "FROM":
				seenFrom = true
			case "ORDER":
				hasOrder = true
				orderIdx = i
			case "IS":
				if !seenFrom {
					return true // 投影里的 IS NULL
				}
			case "NOT":
				if i+1 < len(p.toks) {
					n := p.toks[i+1]
					if n.is("LIKE") || n.is("IN") || n.is("BETWEEN") || n.is("REGEXP") {
						return true
					}
				}
			}
			continue
		}
		// 非关键字（词表外的语法词）：JOIN / DISTINCT / IS / NOT 等
		switch {
		case t.is("JOIN"), t.is("STRAIGHT_JOIN"):
			return true
		case t.is("DISTINCT"), t.is("GROUP"), t.is("HAVING"), t.is("UNION"):
			return true
		case t.is("FROM"):
			seenFrom = true
		case t.is("IS"):
			if !seenFrom {
				return true
			}
		case t.is("ORDER"):
			hasOrder = true
			orderIdx = i
		case t.is("NOT"):
			if i+1 < len(p.toks) {
				n := p.toks[i+1]
				if n.is("LIKE") || n.is("IN") || n.is("BETWEEN") || n.is("REGEXP") {
					return true
				}
			}
		}
	}
	if hasOrder {
		return p.orderByNeedsGeneral(orderIdx)
	}
	return false
}

// orderByNeedsGeneral 判断 ORDER BY 是否引用了投影之外的列
// （快路径只能按输出列排序，其余需要通用路径）。
func (p *sqlParser) orderByNeedsGeneral(orderIdx int) bool {
	fromIdx := -1
	for i := p.i; i < orderIdx; i++ {
		if p.toks[i].is("FROM") {
			fromIdx = i
			break
		}
	}
	if fromIdx < 0 {
		return false
	}
	proj := map[string]bool{}
	for i := p.i; i < fromIdx; i++ {
		if t := p.toks[i]; t.kind == kindIdent || t.kind == kindKw {
			proj[strings.ToLower(t.val)] = true
		}
	}
	j := orderIdx + 1
	if j < len(p.toks) && p.toks[j].is("BY") {
		j++
	}
	for j < len(p.toks) {
		t := p.toks[j]
		if t.is("LIMIT") || t.is("UNION") || t.kind == kindEof {
			break
		}
		if t.kind == kindIdent || t.kind == kindKw {
			if !proj[strings.ToLower(t.val)] {
				return true
			}
		}
		// 跳到下一个 ORDER BY 项
		for j < len(p.toks) && !(p.toks[j].kind == kindSym && p.toks[j].val == ",") {
			if p.toks[j].is("LIMIT") || p.toks[j].is("UNION") || p.toks[j].kind == kindEof {
				return false
			}
			j++
		}
		j++
	}
	return false
}

// effItem 展开 * 之后的实际输出项。
type effItem struct {
	e    *expr
	name string
}

// expandItems 把 selItem 里的 * 展开成具体列。
func (pl *selectPlan) expandItems(metas []*TableMeta) ([]effItem, error) {
	out := make([]effItem, 0, len(pl.items)+4)
	for _, it := range pl.items {
		if !it.star {
			out = append(out, effItem{e: it.e, name: it.name})
			continue
		}
		matched := false
		for i, m := range metas {
			if m == nil {
				continue
			}
			if it.qual != "" && !strings.EqualFold(it.qual, pl.tables[i].alias) && !strings.EqualFold(it.qual, pl.tables[i].name) {
				continue
			}
			for _, f := range m.Fields {
				e := &expr{kind: exCol, col: f.Name, raw: f.Name}
				name := f.Name
				if len(pl.tables) > 1 {
					e.table = pl.tables[i].alias
					e.raw = pl.tables[i].alias + "." + f.Name
					name = e.raw
				}
				out = append(out, effItem{e: e, name: name})
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("unknown table in %s.*", it.qual)
		}
	}
	return out, nil
}

// execSelectPlan 执行查询计划。
func (db *DB) execSelectPlan(p *sqlParser, pl *selectPlan) ([]string, [][]interface{}, error) {
	schema := p.curSchema(db)
	// 第一阶段：只解析表与元数据（不取行）——列校验必须在取行之前，
	// 否则可下推条件已被摘走，未知列会被静默当成空结果。
	var (
		tables  []*Table
		metas   []*TableMeta
		virtual [][]map[string]interface{}
	)
	for _, ft := range pl.tables {
		lower := strings.ToLower(ft.name)
		if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") {
			cols, vrows, verr := db.virtualTableRows(ft.name)
			if verr != nil {
				return nil, nil, verr
			}
			m := &TableMeta{Name: ft.alias}
			for _, c := range cols {
				m.Fields = append(m.Fields, Field{Name: c, Type: TypeVarchar, Len: 255})
			}
			metas = append(metas, m)
			virtual = append(virtual, rowsToMaps(cols, vrows))
			tables = append(tables, nil)
			continue
		}
		phys := db.qualifyTableIn(ft.name, schema)
		t := db.getTable(phys)
		if t == nil {
			return nil, nil, fmt.Errorf("table not found: %s", ft.name)
		}
		metas = append(metas, t.Meta())
		virtual = append(virtual, nil)
		tables = append(tables, t)
	}
	if len(pl.tables) == 0 {
		metas = []*TableMeta{nil}
		virtual = [][]map[string]interface{}{{}}
		tables = []*Table{nil}
	}
	items, err := pl.expandItems(metas)
	if err != nil {
		return nil, nil, err
	}
	resolveOrderByOrdinals(pl, items)
	if err := validatePlanCols(pl, items, metas); err != nil {
		return nil, nil, err
	}
	// 第二阶段：取行（单表会尝试把 WHERE 下推成索引/范围条件）
	rowSets := make([][]map[string]interface{}, len(tables))
	for i, t := range tables {
		if t == nil {
			rowSets[i] = virtual[i]
			continue
		}
		rowSets[i] = db.tableRowMaps(t, pl)
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}

	var joined []map[string]interface{}
	if len(pl.tables) == 0 {
		// 无 FROM 的标量查询（SELECT YEAR('2024-01-01') 等）：单行空行求值
		joined = []map[string]interface{}{{}}
	} else if len(pl.tables) == 1 {
		joined = rowSets[0]
	} else {
		joined = joinRows(rowSets, pl)
	}
	if pl.where != nil {
		filtered := make([]map[string]interface{}, 0, len(joined))
		for _, r := range joined {
			if truthy(pl.where.eval(r)) {
				filtered = append(filtered, r)
			}
		}
		joined = filtered
	}

	var out [][]interface{}
	needAgg := len(pl.groupBy) > 0
	if !needAgg {
		for _, it := range items {
			if exprHasAgg(it.e) {
				needAgg = true
				break
			}
		}
	}
	if !needAgg && pl.having != nil && exprHasAgg(pl.having) {
		needAgg = true
	}
	if needAgg {
		// 无显式 GROUP BY 时：整组一个分组（MySQL 的隐式聚合语义）
		var reps []map[string]interface{}
		out, reps = db.groupAndProject(pl, joined, items)
		// 分组后按 ORDER BY 排序：joined 与分组结果不对齐，必须用每组代表行
		if len(pl.orderBy) > 0 {
			out = sortByExprs(out, reps, pl)
		}
	} else {
		out = make([][]interface{}, 0, len(joined))
		for _, r := range joined {
			row := make([]interface{}, len(items))
			for i, it := range items {
				row[i] = it.e.eval(r)
			}
			out = append(out, row)
		}
		if pl.having != nil {
			kept := out[:0]
			for i, r := range joined {
				if truthy(pl.having.evalGroup(r, joined, map[*expr]interface{}{})) {
					kept = append(kept, out[i])
				}
			}
			out = kept
		}
	}
	if pl.distinct {
		out = dedupRows(out)
	}
	if len(pl.orderBy) > 0 {
		out = sortByExprs(out, joined, pl)
	}
	if pl.offset > 0 {
		if int64(len(out)) <= pl.offset {
			out = nil
		} else {
			out = out[pl.offset:]
		}
	}
	if pl.limit >= 0 && len(out) > pl.limit {
		out = out[:pl.limit]
	}
	return names, out, nil
}

// orderByOutputName 判断 ORDER BY 表达式是否是输出列的别名/列名。
func orderByOutputName(e *expr, items []effItem) (string, bool) {
	if e == nil || e.kind != exCol {
		return "", false
	}
	for _, it := range items {
		if strings.EqualFold(it.name, e.col) {
			return it.name, true
		}
	}
	return "", false
}

// resolveOrderByOrdinals 支持 `ORDER BY 1`（按输出列序号排序）。
func resolveOrderByOrdinals(pl *selectPlan, items []effItem) {
	for i, oe := range pl.orderBy {
		if oe == nil || oe.kind != exConst {
			continue
		}
		n, ok := toFloat(oe.val)
		if !ok || n != float64(int(n)) || int(n) < 1 || int(n) > len(items) {
			continue
		}
		pl.orderBy[i] = items[int(n)-1].e
	}
}

// validatePlanCols 校验表达式里引用的列是否存在（MySQL 对未知列直接报错，
// 静默按 NULL 处理会返回错结果，例如 `WHERE nope=1` 变成全表/空表）。
func validatePlanCols(pl *selectPlan, items []effItem, metas []*TableMeta) error {
	if len(metas) == 0 {
		return nil
	}
	type colSet struct {
		names map[string]bool
		alias map[string]bool
	}
	sets := make([]colSet, len(metas))
	for i, m := range metas {
		cs := colSet{names: map[string]bool{}, alias: map[string]bool{}}
		if m != nil {
			for _, f := range m.Fields {
				cs.names[lowerKey(f.Name)] = true
			}
		}
		if i < len(pl.tables) {
			cs.alias[lowerKey(pl.tables[i].alias)] = true
			cs.alias[lowerKey(pl.tables[i].name)] = true
		}
		sets[i] = cs
	}
	var check func(e *expr) error
	check = func(e *expr) error {
		if e == nil {
			return nil
		}
		if e.kind == exCol {
			found := false
			for _, cs := range sets {
				if e.table != "" {
					if cs.alias[lowerKey(e.table)] && cs.names[lowerKey(e.col)] {
						found = true
						break
					}
					continue
				}
				if cs.names[lowerKey(e.col)] {
					found = true
					break
				}
			}
			if !found {
				if e.table != "" {
					return fmt.Errorf("unknown column '%s.%s'", e.table, e.col)
				}
				return fmt.Errorf("unknown column '%s'", e.col)
			}
			return nil
		}
		for _, a := range e.args {
			if err := check(a); err != nil {
				return err
			}
		}
		for _, w := range e.when {
			if err := check(w); err != nil {
				return err
			}
		}
		return check(e.els)
	}
	for _, it := range items {
		if err := check(it.e); err != nil {
			return err
		}
	}
	if err := check(pl.where); err != nil {
		return err
	}
	for _, g := range pl.groupBy {
		if err := check(g); err != nil {
			return err
		}
	}
	if err := check(pl.having); err != nil {
		return err
	}
	for _, o := range pl.orderBy {
		// ORDER BY 可以按输出别名排序：先按表列校验，失败再按输出列名放过
		if err := check(o); err != nil {
			if oe, ok := orderByOutputName(o, items); ok {
				_ = oe
				continue
			}
			return err
		}
	}
	for _, o := range pl.on {
		if err := check(o); err != nil {
			return err
		}
	}
	return nil
}

// virtualTableRows 读取 information_schema.* / mysql.* 虚拟表。
func (db *DB) virtualTableRows(name string) ([]string, [][]interface{}, error) {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "information_schema.") {
		return db.queryInfoSchema(strings.TrimPrefix(lower, "information_schema."), nil, nil, nil)
	}
	cols, rows, _, _, err := db.queryMysqlTable(strings.TrimPrefix(lower, "mysql."), nil, nil, nil)
	return cols, rows, err
}

// rowsToMaps 把 (列名, 行) 转成 map 行（键为原始列名）。
func rowsToMaps(cols []string, rows [][]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			if i < len(r) {
				m[c] = r[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// tableRowMaps 取一张表的行（map 形式）。单表且 WHERE 可部分下推时复用索引/范围优化。
func (db *DB) tableRowMaps(t *Table, pl *selectPlan) []map[string]interface{} {
	meta := t.Meta()
	if len(pl.tables) == 1 && pl.where != nil {
		conds, filters, rest := extractConds(pl.where)
		if len(conds) > 0 || len(filters) > 0 {
			pl.where = rest
			rows, _ := t.SelectR(conds, filters, nil, nil, -1)
			return rows
		}
	}
	out := make([]map[string]interface{}, 0, 64)
	now := time.Now().UnixNano()
	t.pkTree.scanRangeStop(nil, nil, func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			return true
		}
		row, _, _ := decodeRow(meta, value)
		out = append(out, row)
		return true
	})
	return out
}

func joinRows(rowSets [][]map[string]interface{}, pl *selectPlan) []map[string]interface{} {
	acc := []map[string]interface{}{{}}
	for i, set := range rowSets {
		alias := pl.tables[i].alias
		var on *expr
		if i < len(pl.on) {
			on = pl.on[i]
		}
		next := make([]map[string]interface{}, 0, len(acc)*len(set))
		for _, left := range acc {
			matched := false
			for _, right := range set {
				merged := mergeRow(left, right, alias)
				if on == nil || truthy(on.eval(merged)) {
					next = append(next, merged)
					matched = true
				}
			}
			// 左/右连接：无匹配也保留左行（右表列取 NULL）；INNER/CROSS 不保留
			leftJoin := i < len(pl.joinTypes) && (pl.joinTypes[i] == "LEFT" || pl.joinTypes[i] == "RIGHT")
			if !matched && on != nil && leftJoin {
				next = append(next, mergeRow(left, nil, alias))
			}
		}
		acc = next
	}
	return acc
}

// mergeRow 合并左右行：保留裸列名（小写）与 "别名.列" 两种键。
func mergeRow(a, b map[string]interface{}, alias string) map[string]interface{} {
	out := make(map[string]interface{}, len(a)+len(b)*2)
	for k, v := range a {
		out[k] = v
	}
	if b == nil {
		return out
	}
	for k, v := range b {
		lk := lowerKey(k)
		if _, exists := out[lk]; !exists {
			out[lk] = v
		}
		out[lowerKey(alias+"."+k)] = v
	}
	return out
}

// extractConds 从 WHERE 表达式中提取可下推的等值/范围条件，返回剩余表达式。
func extractConds(e *expr) (map[string]interface{}, []RangeCond, *expr) {
	if e == nil {
		return nil, nil, nil
	}
	if e.kind == exFunc && e.fn == "AND" {
		conds, filters, rest := extractConds(e.args[0])
		c2, f2, r2 := extractConds(e.args[1])
		conds = mergeConds(conds, c2)
		filters = append(filters, f2...)
		return conds, filters, andExpr(rest, r2)
	}
	if e.kind == exFunc && isCmpOp(e.fn) && len(e.args) == 2 {
		if col, ok := plainCol(e.args[0]); ok {
			if val, ok2 := constVal(e.args[1]); ok2 {
				switch e.fn {
				case "=":
					return map[string]interface{}{col: val}, nil, nil
				case ">", ">=", "<", "<=", "<>", "!=":
					return nil, []RangeCond{{Field: col, Op: rangeOp(e.fn), Val: val}}, nil
				}
			}
		}
	}
	return nil, nil, e
}

func mergeConds(a, b map[string]interface{}) map[string]interface{} {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

func andExpr(a, b *expr) *expr {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &expr{kind: exFunc, fn: "AND", args: []*expr{a, b}}
}

func isCmpOp(fn string) bool {
	switch fn {
	case "=", ">", ">=", "<", "<=", "<>", "!=":
		return true
	}
	return false
}

func plainCol(e *expr) (string, bool) {
	if e.kind == exCol && e.table == "" {
		return e.col, true
	}
	return "", false
}

func constVal(e *expr) (interface{}, bool) {
	if e.kind == exConst {
		return e.val, true
	}
	return nil, false
}

// groupAndProject 分组聚合 + 投影；同时返回每组的代表行（供 ORDER BY 使用）。
func (db *DB) groupAndProject(pl *selectPlan, rows []map[string]interface{}, items []effItem) (out [][]interface{}, reps []map[string]interface{}) {
	type grp struct {
		rows []map[string]interface{}
	}
	groups := make(map[string]*grp, 8)
	order := make([]*grp, 0, 8)
	for _, r := range rows {
		key := groupKey(pl.groupBy, r)
		g := groups[key]
		if g == nil {
			g = &grp{}
			groups[key] = g
			order = append(order, g)
		}
		g.rows = append(g.rows, r)
	}
	out = make([][]interface{}, 0, len(order))
	reps = make([]map[string]interface{}, 0, len(order))
	for _, g := range order {
		memo := map[*expr]interface{}{}
		row := make([]interface{}, len(items))
		for i, it := range items {
			row[i] = it.e.evalGroup(g.rows[0], g.rows, memo)
		}
		if pl.having != nil && !truthy(pl.having.evalGroup(g.rows[0], g.rows, memo)) {
			continue
		}
		out = append(out, row)
		reps = append(reps, g.rows[0])
	}
	return out, reps
}

func groupKey(groupBy []*expr, row map[string]interface{}) string {
	parts := make([]string, 0, len(groupBy))
	for _, g := range groupBy {
		v := g.eval(row)
		parts = append(parts, fmt.Sprint(v)+"|"+typeTag(v))
	}
	return strings.Join(parts, "\x00")
}

// sortByExprs 按 ORDER BY 表达式排序（要求 out 与 joined 同序对齐）。
func sortByExprs(out [][]interface{}, joined []map[string]interface{}, pl *selectPlan) [][]interface{} {
	n := len(out)
	if len(joined) != n {
		return out
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		for k, oe := range pl.orderBy {
			c := cmpVal(oe.eval(joined[idx[i]]), oe.eval(joined[idx[j]]))
			if c == 0 {
				continue
			}
			if pl.desc[k] {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	res := make([][]interface{}, n)
	for i, id := range idx {
		res[i] = out[id]
	}
	return res
}

func dedupRows(rows [][]interface{}) [][]interface{} {
	out := make([][]interface{}, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = fmt.Sprint(v) + "|" + typeTag(v)
		}
		k := strings.Join(parts, "\x00")
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}
