package main

import (
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ==================== SQL 解析与执行 ====================

// sqlToken 词法单元。
// upper 缓存标识符/关键字的大写形式，避免后续解析重复 strings.ToUpper。
type sqlToken struct {
	kind  string // "kw" 关键字, "ident" 标识符, "str" 字符串, "num" 数字, "sym" 符号, "eof"
	val   string
	upper string // 仅对 ident/kw 非空
	pos   int
}

// sqlTokenPool 复用 []sqlToken 切片，减少 tokenizeSQL 热路径分配。
var sqlTokenPool = sync.Pool{
	New: func() interface{} { return make([]sqlToken, 0, 16) },
}

// interfaceSlicePool 复用 []interface{} 切片，用于行构建等临时场景。
var interfaceSlicePool = sync.Pool{
	New: func() interface{} { return make([]interface{}, 0, 8) },
}

// tokenizeSQL SQL 分词：关键字/标识符、单引号字符串、整数、符号。
// 预分配 tokens 切片并缓存关键字大写字面量，降低热路径分配。
func tokenizeSQL(sql string) ([]sqlToken, error) {
	toks := sqlTokenPool.Get().([]sqlToken)[:0]
	i := 0
	n := len(sql)
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			j := i + 1
			escaped := false
			for j < n {
				if sql[j] == '\'' {
					if j+1 < n && sql[j+1] == '\'' {
						escaped = true
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= n {
				sqlTokenPool.Put(toks[:0])
				return nil, fmt.Errorf("unterminated string literal")
			}
			// 无 '' 转义时直接返回子串（零分配）；有转义才展开。
			var val string
			if !escaped {
				val = sql[i+1 : j]
			} else {
				var sb strings.Builder
				for k := i + 1; k < j; k++ {
					if sql[k] == '\'' && k+1 < j && sql[k+1] == '\'' {
						sb.WriteByte('\'')
						k++
						continue
					}
					sb.WriteByte(sql[k])
				}
				val = sb.String()
			}
			toks = append(toks, sqlToken{kind: "str", val: val, pos: i})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '-' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9'):
			j := i
			if c == '-' {
				j++
			}
			for j < n && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			toks = append(toks, sqlToken{kind: "num", val: sql[i:j], pos: i})
			i = j
		case isIdentChar(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			word := sql[i:j]
			upper := strings.ToUpper(word)
			kind := "ident"
			if isKeyword(upper) {
				kind = "kw"
			}
			toks = append(toks, sqlToken{kind: kind, val: word, upper: upper, pos: i})
			i = j
		default:
			toks = append(toks, sqlToken{kind: "sym", val: symChar(c), pos: i})
			i++
		}
	}
	toks = append(toks, sqlToken{kind: "eof", val: "", pos: n})
	return toks, nil
}

// symChar 返回单字符符号的缓存字符串，避免 string(c) 每次分配。
var symCache = func() [256]string {
	var a [256]string
	for i := range a {
		a[i] = string(rune(i))
	}
	return a
}()

func symChar(c byte) string {
	if c < 128 {
		return symCache[c]
	}
	return string(rune(c))
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isKeyword 判断大写字符序列是否为保留关键字。
func isKeyword(u string) bool {
	switch u {
	case "SHOW", "TABLES", "DESCRIBE", "DESC", "SELECT", "FROM", "WHERE", "AND", "LIMIT",
		"INSERT", "INTO", "VALUES", "UPDATE", "SET", "DELETE", "CREATE", "TABLE", "DROP",
		"PRIMARY", "KEY", "INT", "INTEGER", "INT64", "BIGINT", "VARCHAR", "STRING", "TEXT",
		"CHAR", "BOOL", "BOOLEAN", "LIKE", "BETWEEN":
		return true
	}
	return false
}

type sqlParser struct {
	toks     []sqlToken
	i        int
	params   []interface{}   // 非 nil 时表示处于参数绑定模式（prepared execute）
	paramIdx int            // 已消费的参数下标
	txnID    uint64         // 当前事务 ID（0 表示自动提交）
}

// hasParams 报告解析器是否处于参数绑定模式。
func (p *sqlParser) hasParams() bool { return p.params != nil }

// nextParam 返回下一个待绑定参数；超出时返回 nil（调用方按缺失处理）。
func (p *sqlParser) nextParam() interface{} {
	if p.params == nil {
		return nil
	}
	if p.paramIdx >= len(p.params) {
		return nil
	}
	v := p.params[p.paramIdx]
	p.paramIdx++
	return v
}

func (p *sqlParser) peek() sqlToken { return p.toks[p.i] }
func (p *sqlParser) next() sqlToken {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

// matchKeyword 匹配关键字/标识符（kw 须为大写字符串），大小写不敏感。
func (p *sqlParser) matchKeyword(kw string) bool {
	t := p.peek()
	if (t.kind == "kw" || t.kind == "ident") && t.upper == kw {
		p.next()
		return true
	}
	return false
}

func (p *sqlParser) matchSym(s string) bool {
	t := p.peek()
	if t.kind == "sym" && t.val == s {
		p.next()
		return true
	}
	return false
}

// runSQL 执行一条 SQL，返回结果集（SELECT）或影响行数描述。
func (db *DB) runSQL(sql string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLP(sql, nil)
}

// runSQLP 执行 SQL；params 非 nil 时启用参数绑定（? 占位符按下标取值），用于 prepared execute。
func (db *DB) runSQLP(sql string, params []interface{}) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLTx(sql, params, 0)
}

// runSQLTx 执行 SQL，与 runSQLP 相同但额外携带事务 ID（0 = 自动提交）。
func (db *DB) runSQLTx(sql string, params []interface{}, txnID uint64) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	toks, err := tokenizeSQL(sql)
	if err != nil {
		return
	}
	defer sqlTokenPool.Put(toks[:0])
	return db.runSQLTokens(toks, sql, params, txnID)
}

// runSQLTokens 使用调用方提供的已分词 tokens 执行（prepared 复用，避免每次 EXECUTE 重新分词）。
func (db *DB) runSQLTokens(toks []sqlToken, sql string, params []interface{}, txnID uint64) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p := &sqlParser{toks: toks, params: params, txnID: txnID}
	// 去除开头的空/分号
	for p.peek().kind == "sym" && p.peek().val == ";" {
		p.next()
	}
	first := p.peek()
	if first.kind == "eof" {
		err = fmt.Errorf("empty query")
		return
	}
	switch first.upper {
	case "SHOW":
		if !p.matchKeyword("SHOW") {
			break
		}
		// SHOW FULL COLUMNS / SHOW COLUMNS FROM tbl [FROM db] [LIKE '..']
		p.matchKeyword("FULL")
		if p.peek().upper == "COLUMNS" {
			p.next()
			if !p.matchKeyword("FROM") {
				err = fmt.Errorf("expected FROM in SHOW COLUMNS")
				return
			}
			tbl, e := p.readTableIdent()
			if e != nil {
				err = e
				return
			}
			dbName := db.getCurDB()
			if p.matchKeyword("FROM") {
				if t := p.next(); t.kind == "ident" || t.kind == "str" {
					dbName = t.val
				}
			}
			columns, rows, err = db.describeTable(dbName, tbl)
			return
		}
		// SHOW TABLE STATUS FROM db [LIKE '..']
		if p.matchKeyword("TABLE") && p.matchKeyword("STATUS") {
			dbName := db.getCurDB()
			if p.matchKeyword("FROM") {
				if d := p.next(); d.kind == "ident" || d.kind == "str" {
					dbName = d.val
				}
			}
			for p.matchKeyword("LIKE") {
				p.next()
			}
			columns, rows, err = db.tableStatus(dbName)
			return
		}
		// SHOW INDEX / SHOW INDEXES / SHOW KEYS FROM tbl [FROM db]
		if p.matchKeyword("INDEX") || p.matchKeyword("INDEXES") || p.matchKeyword("KEYS") {
			if !p.matchKeyword("FROM") {
				err = fmt.Errorf("expected FROM in SHOW INDEX")
				return
			}
			tbl, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			dbName := db.getCurDB()
			if p.matchKeyword("FROM") {
				if d := p.next(); d.kind == "ident" || d.kind == "str" {
					dbName = d.val
				}
			}
			columns, rows, err = db.showIndex(dbName, tbl)
			return
		}
		// SHOW CREATE TABLE / CREATE DATABASE / CREATE VIEW
		if p.matchKeyword("CREATE") {
			if p.matchKeyword("DATABASE") {
				columns = []string{"Database", "Create Database"}
				name := ""
				if p.matchKeyword("IF") {
					p.matchKeyword("NOT")
					p.matchKeyword("EXISTS")
				}
				if d := p.next(); d.kind == "ident" || d.kind == "str" {
					name = d.val
				}
				rows = [][]interface{}{{name, "CREATE DATABASE `" + name + "` /*!40100 DEFAULT CHARACTER SET utf8mb4 */"}}
				return
			}
			p.matchKeyword("VIEW")
			if !p.matchKeyword("TABLE") {
				break
			}
			tbl, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			columns = []string{"Table", "Create Table"}
			rows = [][]interface{}{{tbl, db.createTableStmt(tbl)}}
			return
		}
		// SHOW GRANTS [FOR user@host]
		if p.matchKeyword("GRANTS") {
			grantees := "CURRENT_USER"
			if len(mysqlCfg.get().Users) > 0 {
				grantees = mysqlCfg.get().Users[0].User + "@" + mysqlCfg.get().Users[0].Host
			}
			columns = []string{"Grants for " + grantees}
			for _, u := range mysqlCfg.get().Users {
				rows = append(rows, []interface{}{"GRANT USAGE ON *.* TO `" + u.User + "`@`" + u.Host + "`"})
			}
			return
		}
		// SHOW ENGINES
		if p.matchKeyword("ENGINES") {
			columns, rows, err = db.queryInfoSchema("ENGINES", nil, nil)
			return
		}
		// SHOW PLUGINS
		if p.matchKeyword("PLUGINS") {
			columns, rows, err = db.queryInfoSchema("PLUGINS", nil, nil)
			return
		}
		// SHOW CHARACTER SET / SHOW CHARSET
		if p.matchKeyword("CHARACTER") {
			p.matchKeyword("SET")
			columns, rows, err = db.queryInfoSchema("CHARACTER_SETS", nil, nil)
			return
		}
		if p.matchKeyword("CHARSET") {
			columns, rows, err = db.queryInfoSchema("CHARACTER_SETS", nil, nil)
			return
		}
		// SHOW COLLATION [LIKE '..']
		if p.matchKeyword("COLLATION") {
			if p.matchKeyword("LIKE") {
				p.next()
			}
			columns, rows, err = db.queryInfoSchema("COLLATIONS", nil, nil)
			return
		}
		// SHOW STATUS / SHOW GLOBAL STATUS / SHOW SESSION STATUS
		if p.matchKeyword("STATUS") {
			columns = []string{"Variable_name", "Value"}
			snap := db.stats.Snapshot()
			for _, k := range []string{"total_commands", "total_errors"} {
				rows = append(rows, []interface{}{strings.ToUpper(k), snap[k]})
			}
			sortRowsByFirst(rows)
			return
		}
		if p.matchKeyword("PROCESSLIST") {
			columns = []string{"Id", "User", "Host", "db", "Command", "Time", "State", "Info"}
			return
		}
		if p.matchKeyword("TRIGGERS") {
			columns = []string{"Trigger", "Event", "Table", "Statement", "Timing", "Created", "sql_mode", "Definer", "character_set_client"}
			return
		}
		if p.matchKeyword("EVENTS") {
			columns = []string{"Event", "Definer", "Time", "Type", "Status"}
			return
		}
		if p.matchKeyword("WARNINGS") || p.matchKeyword("ERRORS") {
			columns = []string{"Level", "Code", "Message"}
			rows = nil
			return
		}
		if p.matchKeyword("TABLES") {
			tables, e := db.adminTables("")
			if e != nil {
				err = e
				return
			}
			cur := db.getCurDB()
			prefix := cur + "."
			columns = []string{"name", "pk", "row_count"}
			for _, tb := range tables {
				n := tb["name"].(string)
				// 当前库过滤：默认库 tsumugi 显示无前缀表；其它库显示 db. 前缀表
				if cur == "tsumugi" {
					if strings.Contains(n, ".") {
						continue
					}
				} else if !strings.HasPrefix(n, prefix) {
					continue
				}
				show := n
				if cur != "tsumugi" {
					show = strings.TrimPrefix(n, prefix)
				}
				rows = append(rows, []interface{}{show, tb["pk"], tb["row_count"]})
			}
			return
		}
		if p.matchKeyword("DATABASES") {
			columns = []string{"Database"}
			for _, d := range mysqlCfg.get().Databases {
				rows = append(rows, []interface{}{d})
			}
			return
		}
		if p.matchKeyword("VARIABLES") {
			like := ""
			if p.matchKeyword("LIKE") {
				if t := p.next(); t.kind == "str" {
					like = t.val
				}
			}
			columns = []string{"Variable_name", "Value"}
			for k, v := range mysqlCfg.get().Variables {
				if like != "" && !likeMatch(k, like) {
					continue
				}
				rows = append(rows, []interface{}{k, v})
			}
			sortRowsByFirst(rows)
			return
		}
		if p.matchKeyword("WARNINGS") || p.matchKeyword("ERRORS") {
			columns = []string{"Level", "Code", "Message"}
			rows = nil
			return
		}
		err = fmt.Errorf("unsupported SHOW")
		return
	case "USE":
		p.next()
		nameTok := p.next()
		if nameTok.kind != "ident" && nameTok.kind != "str" {
			err = fmt.Errorf("expected database name")
			return
		}
		if !mysqlCfg.hasDatabase(nameTok.val) {
			err = fmt.Errorf("unknown database %s", nameTok.val)
			return
		}
		db.setCurDB(nameTok.val)
		rawMsg = "Database changed"
		return
	case "CREATE":
		if p.i+1 < len(p.toks) && p.toks[p.i+1].upper == "DATABASE" {
			p.next() // CREATE
			p.next() // DATABASE
			if p.matchKeyword("IF") {
				p.matchKeyword("NOT")
				p.matchKeyword("EXISTS")
			}
			nameTok := p.next()
			if nameTok.kind != "ident" && nameTok.kind != "str" {
				err = fmt.Errorf("expected database name")
				return
			}
			// 幂等成功：已存在时同样返回 OK（与 IF NOT EXISTS 语义一致），避免“已存在”误报
			mysqlCfg.addDatabase(nameTok.val)
			rawMsg = fmt.Sprintf("Query OK, 1 row affected (database %s)", nameTok.val)
			return
		}
		return db.parseCreateTable(p)
	case "DROP":
		if p.i+1 < len(p.toks) && p.toks[p.i+1].upper == "DATABASE" {
			p.next() // DROP
			p.next() // DATABASE
			ifExists := false
			if p.matchKeyword("IF") {
				p.matchKeyword("EXISTS")
				ifExists = true
			}
			nameTok := p.next()
			if nameTok.kind != "ident" && nameTok.kind != "str" {
				err = fmt.Errorf("expected database name")
				return
			}
			dropped, e := db.dropDatabase(nameTok.val)
			if e != nil {
				err = e
				return
			}
			if dropped {
				rawMsg = fmt.Sprintf("Query OK, database %s dropped", nameTok.val)
			} else if ifExists {
				rawMsg = "Query OK"
			} else {
				err = fmt.Errorf("unknown database %s", nameTok.val)
				return
			}
			return
		}
		return db.parseDropTable(p)
	case "DESCRIBE", "DESC":
		p.next()
		nameTok := p.next()
		if nameTok.kind != "ident" && nameTok.kind != "str" {
			err = fmt.Errorf("expected table name")
			return
		}
		tables, e := db.adminTables("")
		if e != nil {
			err = e
			return
		}
		for _, tb := range tables {
			if tb["name"] == nameTok.val {
				columns = []string{"field", "type", "len", "key"}
				for _, f := range tb["fields"].([]map[string]interface{}) {
					key := ""
					if tb["pk"] == f["name"] {
						key = "PRI"
					}
					rows = append(rows, []interface{}{f["name"], f["type"], f["len"], key})
				}
				for _, ix := range tb["indexes"].([]map[string]interface{}) {
					columns = []string{"field", "type", "len", "key"}
					rows = append(rows, []interface{}{ix["field"], "INDEX(" + ix["name"].(string) + ")", "", "MUL"})
				}
				return
			}
		}
		err = fmt.Errorf("table not found: %s", nameTok.val)
		return
case "SELECT":
		return db.parseSelect(p)
	case "INSERT":
		return db.parseInsert(p)
	case "UPDATE":
		return db.parseUpdate(p)
	case "DELETE":
		return db.parseDelete(p)
	case "ALTER":
		return db.parseAlterTable(p)
	case "SET", "SETS":
		rawMsg = "Query OK"
		return
	}
	err = fmt.Errorf("unsupported statement: %s", first.val)
	return
}

// countSQLTokens 统计 token 列表中的占位符数量（供 prepared 使用）。
func countSQLTokens(toks []sqlToken) int {
	n := 0
	for _, t := range toks {
		if t.kind == "sym" && t.val == "?" {
			n++
		}
	}
	return n
}

func countSQLParams(sql string) int {
	toks, err := tokenizeSQL(sql)
	if err != nil {
		return 0
	}
	defer sqlTokenPool.Put(toks[:0])
	return countSQLTokens(toks)
}

func (p *sqlParser) parseIdent() (string, error) {
	t := p.next()
	if t.kind != "ident" && t.kind != "str" {
		return "", fmt.Errorf("expected identifier, got %q", t.val)
	}
	return t.val, nil
}

// readTableIdent 读取表名/库表名标识符；关键字也可作表名（如 information_schema.TABLES、mysql.KILL）。
func (p *sqlParser) readTableIdent() (string, error) {
	t := p.next()
	if t.kind != "ident" && t.kind != "kw" && t.kind != "str" {
		return "", fmt.Errorf("expected identifier, got %q", t.val)
	}
	return t.val, nil
}

func (p *sqlParser) parseValue() (interface{}, error) {
	t := p.next()
	// 参数绑定占位符：? 从运行参数按下标取值（prepared execute）。
	if t.kind == "sym" && t.val == "?" {
		v := p.nextParam()
		if v == nil {
			return nil, fmt.Errorf("no value bound for parameter %d", p.paramIdx+1)
		}
		return v, nil
	}
	switch t.kind {
	case "num":
		v, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "str":
		return t.val, nil
	case "kw":
		if t.upper == "TRUE" || t.upper == "FALSE" {
			return t.upper == "TRUE", nil
		}
	}
	return nil, fmt.Errorf("unexpected value %q", t.val)
}

func (db *DB) parseSelect(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // SELECT
	// 无 FROM 的标量查询（SELECT @@version, SELECT VERSION(), SELECT NOW() 等）
	if c, r, e := db.queryScalarSelect(p); r != nil {
		columns, rows, err = c, r, e
		return
	}
	// COUNT 聚合：SELECT COUNT(*) FROM tbl [WHERE ..] / SELECT COUNT(1) 等
	isCount := p.peek().upper == "COUNT" && p.toks[p.i+1].kind == "sym" && p.toks[p.i+1].val == "("
	if isCount {
		p.next() // COUNT
		p.next() // (
		inExpr := false
		for {
			t := p.next()
			if t.kind == "sym" && t.val == ")" {
				break
			}
			if t.kind == "eof" {
				err = fmt.Errorf("expected ) in COUNT")
				return
			}
			if t.kind == "ident" {
				inExpr = true
			}
		}
		if !p.matchKeyword("FROM") {
			err = fmt.Errorf("expected FROM")
			return
		}
		_ = inExpr
		columns, rows, affected, rawMsg, err = db.countSelect(p)
		return
	}
	var selectCols []string
	if t := p.next(); t.kind == "sym" && t.val == "*" {
		selectCols = nil // 表示所有列
	} else {
		firstTok := t
		for {
			if firstTok.kind != "ident" && firstTok.kind != "kw" {
				err = fmt.Errorf("expected column name, got %q", firstTok.val)
				return
			}
			selectCols = append(selectCols, firstTok.val)
			if !p.matchSym(",") {
				break
			}
			firstTok = p.next()
		}
	}
	if !p.matchKeyword("FROM") {
		err = fmt.Errorf("expected FROM")
		return
	}
	tableName, e := p.readTableIdent()
	if e != nil {
		err = e
		return
	}
	// 支持 mysql.user 形式（tokenizer 拆成 mysql . user）
	for p.peek().kind == "sym" && p.peek().val == "." {
		p.next()
		part, pe := p.readTableIdent()
		if pe != nil {
			err = pe
			return
		}
		tableName += "." + part
	}
	// 非 mysql.* 且无前缀时按当前数据库解析（仅非默认库加前缀）
	if !strings.HasPrefix(strings.ToLower(tableName), "mysql.") {
		tableName = db.qualifyTable(tableName)
	}
	conds := map[string]interface{}{}
	type rawCond struct {
		field string
		op    string // "=" ">" "<" ">=" "<=" "<>" "BETWEEN"
		val   interface{}
		val2  interface{}
	}
	var rawConds []*rawCond
	if p.matchKeyword("WHERE") {
		for {
			field, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			op := p.next()
			// BETWEEN 是关键字(kw)，其余比较符是符号(sym)
			isBetween := (op.kind == "kw" || op.kind == "ident") && op.upper == "BETWEEN"
			if op.kind != "sym" && !isBetween {
				err = fmt.Errorf("unsupported comparison operator")
				return
			}
			// tokenizer 会把 >= <= <> != 拆成两个单字符符号，这里合并
			if (op.val == ">" || op.val == "<" || op.val == "!") && p.peek().kind == "sym" && p.peek().val == "=" {
				p.next() // 消费 "="，组成 ">=" "<=" "!="
				if op.val == "!" {
					op.val = "<>"
				} else {
					op.val = op.val + "="
				}
			}
			switch strings.ToUpper(op.val) {
			case "=", ">", "<", ">=", "<=", "<>", "!=":
				val, e := p.parseValue()
				if e != nil {
					err = e
					return
				}
				rawConds = append(rawConds, &rawCond{field: field, op: op.val, val: val})
			case "BETWEEN":
				lo, e := p.parseValue()
				if e != nil {
					err = e
					return
				}
				if !p.matchKeyword("AND") {
					err = fmt.Errorf("expected AND in BETWEEN")
					return
				}
				hi, e := p.parseValue()
				if e != nil {
					err = e
					return
				}
				rawConds = append(rawConds, &rawCond{field: field, op: "BETWEEN", val: lo, val2: hi})
			default:
				err = fmt.Errorf("unsupported comparison operator: %s", op.val)
				return
			}
			if !p.matchKeyword("AND") {
				break
			}
		}
	}
	limit := 200
	// 兼容客户端常见子句：ORDER BY / GROUP BY / HAVING（不参与执行，仅消费 token）
	for {
		if p.matchKeyword("ORDER") {
			if p.matchKeyword("BY") {
				if _, e := p.parseIdent(); e != nil {
					err = e
					return
				}
				p.matchKeyword("ASC")
				p.matchKeyword("DESC")
				for p.matchSym(",") {
					if _, e := p.parseIdent(); e != nil {
						err = e
						return
					}
					p.matchKeyword("ASC")
					p.matchKeyword("DESC")
				}
				continue
			}
			continue
		}
		if p.matchKeyword("GROUP") {
			p.matchKeyword("BY")
			for {
				if _, e := p.parseIdent(); e != nil {
					err = e
					return
				}
				if !p.matchSym(",") {
					break
				}
			}
			continue
		}
		if p.matchKeyword("HAVING") {
			if _, e := p.parseIdent(); e != nil {
				err = e
				return
			}
			continue
		}
		break
	}
	if p.matchKeyword("LIMIT") {
		num := p.next()
		if num.kind != "num" {
			err = fmt.Errorf("expected number after LIMIT")
			return
		}
		v, e := strconv.Atoi(num.val)
		if e != nil {
			err = e
			return
		}
		limit = v
	}
	// mysql.* 虚拟系统表
	if strings.HasPrefix(strings.ToLower(tableName), "mysql.") {
		mt := strings.ToLower(strings.TrimPrefix(tableName, "mysql."))
		for _, c := range rawConds {
			if c.op == "=" {
				conds[c.field] = c.val
			}
		}
		return db.queryMysqlTable(mt, conds, selectCols)
	}
	// information_schema.* 虚拟系统表
	if strings.HasPrefix(strings.ToLower(tableName), "information_schema.") {
		it := strings.ToLower(strings.TrimPrefix(tableName, "information_schema."))
		for _, c := range rawConds {
			if c.op == "=" {
				conds[c.field] = c.val
			}
		}
		columns, rows, err = db.queryInfoSchema(it, conds, selectCols)
		return
	}
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	// 拆分原始条件为：等值(map) + 主键范围(min/max) + 非主键范围过滤
	var minKey, maxKey *int64
	var filters []RangeCond
	for _, c := range rawConds {
		if c.op == "=" {
			conds[c.field] = c.val
			continue
		}
		if strings.EqualFold(c.field, t.meta.PK) {
			switch c.op {
			case ">":
				iv := toInt64(c.val)
				var lo int64
				if iv >= math.MaxInt64 {
					lo = math.MaxInt64
				} else {
					lo = iv + 1
				}
				if minKey == nil || *minKey < lo {
					minKey = &lo
				}
			case ">=":
				iv := toInt64(c.val)
				if minKey == nil || *minKey < iv {
					minKey = &iv
				}
			case "<":
				iv := toInt64(c.val)
				var hi int64
				if iv <= math.MinInt64 {
					hi = math.MinInt64
				} else {
					hi = iv - 1
				}
				if maxKey == nil || *maxKey > hi {
					maxKey = &hi
				}
			case "<=":
				iv := toInt64(c.val)
				if maxKey == nil || *maxKey > iv {
					maxKey = &iv
				}
			case "<>":
				err = fmt.Errorf("unable to use <> on primary key")
				return
			case "BETWEEN":
				lo := toInt64(c.val)
				hi := toInt64(c.val2)
				if minKey == nil || *minKey < lo {
					minKey = &lo
				}
				if maxKey == nil || *maxKey > hi {
					maxKey = &hi
				}
			}
			continue
		}
		filters = append(filters, RangeCond{Field: c.field, Op: rangeOp(c.op), Val: c.val, Val2: c.val2})
	}
	// 列投影：SELECT * 返回全部列；否则只返回指定的列
	cols := t.meta.Fields
	if selectCols != nil {
		cols = nil
		for _, sc := range selectCols {
			for _, f := range t.meta.Fields {
				if strings.EqualFold(sc, f.Name) {
					cols = append(cols, f)
					break
				}
			}
		}
		if len(cols) == 0 {
			err = fmt.Errorf("no matching columns")
			return
		}
	}
	columns = make([]string, 0, len(cols))
	for _, f := range cols {
		columns = append(columns, f.Name)
	}
	// 无 WHERE 条件的快速路径：直接解码为 []interface{}，跳过每行 map 分配
	if len(conds) == 0 && len(filters) == 0 {
		flat := t.selectRowsFlat(minKey, maxKey, limit)
		if selectCols == nil {
			rows = flat
			return
		}
		// 列投影：flat 行按 meta.Fields 顺序排列，按下标取值
		idx := make([]int, 0, len(cols))
		for _, f := range cols {
			for i, mf := range t.meta.Fields {
				if mf.Name == f.Name {
					idx = append(idx, i)
					break
				}
			}
		}
		rows = make([][]interface{}, 0, len(flat))
		for _, r := range flat {
			row := make([]interface{}, 0, len(idx))
			for _, i := range idx {
				row = append(row, r[i])
			}
			rows = append(rows, row)
			if len(rows) >= limit {
				break
			}
		}
		return
	}
	res, e := t.SelectR(conds, filters, minKey, maxKey, limit)
	if e != nil {
		err = e
		return
	}
	for _, r := range res {
		row := make([]interface{}, 0, len(cols))
		for _, f := range cols {
			row = append(row, r[f.Name])
		}
		rows = append(rows, row)
		if len(rows) >= limit {
			break
		}
	}
	return
}

// toInt64 将接口值转为 int64。数字直接取，其他返回 0。
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// rangeOp 将 SQL 比较符映射为 RangeCond 使用的 op 常量。
func rangeOp(op string) string {
	switch op {
	case ">":
		return "gt"
	case ">=":
		return "ge"
	case "<":
		return "lt"
	case "<=":
		return "le"
	case "<>", "!=":
		return "ne"
	case "BETWEEN":
		return "between"
	}
	return "="
}

// queryMysqlTable 处理 SELECT ... FROM mysql.<table> 虚拟系统表。
func (db *DB) queryMysqlTable(name string, conds map[string]interface{}, selectCols []string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	mc := mysqlCfg.get()
	project := func(rec map[string]interface{}) []interface{} {
		if selectCols == nil {
			row := make([]interface{}, 0, len(rec))
			for _, k := range []string{"user", "host", "plugin"} {
				row = append(row, rec[k])
			}
			return row
		}
		row := make([]interface{}, 0, len(selectCols))
		for _, c := range selectCols {
			row = append(row, rec[strings.ToLower(c)])
		}
		return row
	}
	header := func() []string {
		if selectCols == nil {
			return []string{"user", "host", "plugin"}
		}
		h := make([]string, 0, len(selectCols))
		for _, c := range selectCols {
			h = append(h, strings.ToLower(c))
		}
		return h
	}
	matchCond := func(rec map[string]interface{}) bool {
		for k, v := range conds {
			if rec[strings.ToLower(k)] != v {
				return false
			}
		}
		return true
	}

	switch name {
	case "user":
		columns = header()
		for _, u := range mc.Users {
			rec := map[string]interface{}{"user": u.User, "host": u.Host, "plugin": u.Plugin}
			if matchCond(rec) {
				rows = append(rows, project(rec))
			}
		}
		return
	case "db", "tables_priv", "columns_priv", "procs_priv", "global_grants":
		columns = header()
		return
	case "general_log", "slow_log":
		columns = header()
		return
	}
	err = fmt.Errorf("table 'mysql.%s' doesn't exist", name)
	return
}

func (db *DB) parseInsert(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // INSERT
	if !p.matchKeyword("INTO") {
		err = fmt.Errorf("expected INTO")
		return
	}
	tableName, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTable(tableName)
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	var fields []string
	colsExplicit := false
	if p.peek().kind == "sym" && p.peek().val == "(" {
		colsExplicit = true
		p.next()
		for {
			f, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			fields = append(fields, f)
			if p.matchSym(")") {
				break
			}
			if q := p.next(); q.kind != "sym" || q.val != "," {
				err = fmt.Errorf("expected , or )")
				return
			}
		}
	}
	if !p.matchKeyword("VALUES") {
		err = fmt.Errorf("expected VALUES")
		return
	}
	// 多行 INSERT：INSERT INTO t VALUES (..),(..),(..)
	affected = 0
	for {
		if q := p.next(); q.kind != "sym" || q.val != "(" {
			err = fmt.Errorf("expected (")
			return
		}
		var values []interface{}
		if colsExplicit {
			values = make([]interface{}, 0, len(fields))
		} else {
			values = interfaceSlicePool.Get().([]interface{})[:0]
			if cap(values) < len(t.meta.Fields) {
				values = make([]interface{}, 0, len(t.meta.Fields))
			}
		}
		for {
			v, e := p.parseValue()
			if e != nil {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				err = e
				return
			}
			values = append(values, v)
			if p.matchSym(")") {
				break
			}
			if q := p.next(); q.kind != "sym" || q.val != "," {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				err = fmt.Errorf("expected , or )")
				return
			}
		}
		// 提交单行
		if e := db.insertRow(p, t, fields, colsExplicit, values); e != nil {
			if !colsExplicit {
				interfaceSlicePool.Put(values[:0])
			}
			err = e
			return
		}
		if !colsExplicit {
			interfaceSlicePool.Put(values[:0])
		}
		affected++
		// 下一行：逗号后必须是 (；否则结束
		if !p.matchSym(",") {
			break
		}
		if p.peek().kind == "sym" && p.peek().val == "(" {
			continue
		}
		break
	}
	rawMsg = fmt.Sprintf("%d row inserted", affected)
	if affected > 1 {
		rawMsg = fmt.Sprintf("%d rows inserted", affected)
	}
	return
}

// insertRow 提交 INSERT 的单行：优先位置参数快速路径，否则按列名映射。
func (db *DB) insertRow(p *sqlParser, t *Table, fields []string, colsExplicit bool, values []interface{}) error {
	// 快速路径：位置参数（字段序即表字段序）直接编码，跳过 map 构建
	pkIdx := -1
	if colsExplicit {
		if len(fields) != len(values) {
			return fmt.Errorf("column count mismatch")
		}
		for i, f := range fields {
			if f == t.meta.PK {
				pkIdx = i
				break
			}
		}
	} else {
		if len(values) != len(t.meta.Fields) {
			return fmt.Errorf("column count mismatch")
		}
		for i, f := range t.meta.Fields {
			if f.Name == t.meta.PK {
				pkIdx = i
				break
			}
		}
	}
	if pkIdx >= 0 && pkIdx < len(values) {
		pk, ok := values[pkIdx].(int64)
		if !ok {
			return fmt.Errorf("primary key must be integer")
		}
		return t.InsertOrdered(pk, values, 0, p.txnID, false)
	}
	if colsExplicit {
		rowMap := make(map[string]interface{}, len(fields))
		for i, f := range fields {
			rowMap[f] = values[i]
		}
		pkVal, ok := rowMap[t.meta.PK]
		if !ok {
			return fmt.Errorf("missing primary key field %s", t.meta.PK)
		}
		pk, ok := pkVal.(int64)
		if !ok {
			return fmt.Errorf("primary key must be integer")
		}
		return t.Insert(pk, rowMap, 0, p.txnID, false)
	}
	return fmt.Errorf("missing primary key field %s", t.meta.PK)
}

func (db *DB) parseUpdate(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // UPDATE
	tableName, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTable(tableName)
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	if !p.matchKeyword("SET") {
		err = fmt.Errorf("expected SET")
		return
	}
	updates := map[string]interface{}{}
	for {
		f, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		if op := p.next(); op.kind != "sym" || op.val != "=" {
			err = fmt.Errorf("expected =")
			return
		}
		v, e := p.parseValue()
		if e != nil {
			err = e
			return
		}
		updates[f] = v
		if !p.matchSym(",") {
			break
		}
	}
	if !p.matchKeyword("WHERE") {
		err = fmt.Errorf("UPDATE requires WHERE pk=...")
		return
	}
	pkField, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	if op := p.next(); op.kind != "sym" || op.val != "=" {
		err = fmt.Errorf("expected =")
		return
	}
	pkVal, e := p.parseValue()
	if e != nil {
		err = e
		return
	}
	if pkField != t.meta.PK {
		err = fmt.Errorf("UPDATE WHERE must use primary key %s", t.meta.PK)
		return
	}
	pk, ok := pkVal.(int64)
	if !ok {
		err = fmt.Errorf("primary key must be integer")
		return
	}
	old, e := t.Select(map[string]interface{}{t.meta.PK: pk}, nil, nil)
	if e != nil {
		err = e
		return
	}
	if len(old) == 0 {
		err = fmt.Errorf("row not found")
		return
	}
	full := map[string]interface{}{}
	for k, v := range old[0] {
		full[k] = v
	}
	for k, v := range updates {
		full[k] = v
	}
	if e := t.Update(pk, full, 0, p.txnID, false); e != nil {
		err = e
		return
	}
	affected = 1
	rawMsg = "1 row updated"
	return
}

func (db *DB) parseDelete(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // DELETE
	if !p.matchKeyword("FROM") {
		err = fmt.Errorf("expected FROM")
		return
	}
	tableName, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTable(tableName)
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	if !p.matchKeyword("WHERE") {
		err = fmt.Errorf("DELETE requires WHERE pk=...")
		return
	}
	pkField, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	if op := p.next(); op.kind != "sym" || op.val != "=" {
		err = fmt.Errorf("expected =")
		return
	}
	pkVal, e := p.parseValue()
	if e != nil {
		err = e
		return
	}
	if pkField != t.meta.PK {
		err = fmt.Errorf("DELETE WHERE must use primary key %s", t.meta.PK)
		return
	}
	pk, ok := pkVal.(int64)
	if !ok {
		err = fmt.Errorf("primary key must be integer")
		return
	}
	if e := t.Delete(pk, p.txnID, false); e != nil {
		err = e
		return
	}
	affected = 1
	rawMsg = "1 row deleted"
	return
}

func (db *DB) parseCreateTable(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // CREATE
	if !p.matchKeyword("TABLE") {
		err = fmt.Errorf("expected TABLE")
		return
	}
	ifExists := false
	if p.matchKeyword("IF") {
		p.matchKeyword("NOT")
		ifExists = true
		p.matchKeyword("EXISTS")
	}
	name, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	name = db.qualifyTable(name)
	if ifExists {
		if db.getTable(name) != nil {
			rawMsg = "Query OK"
			return
		}
	}
	if q := p.next(); q.kind != "sym" || q.val != "(" {
		err = fmt.Errorf("expected (")
		return
	}
	var fields []Field
	var pkName string
	pkNameSet := false
	for {
		// 允许 PRIMARY KEY (col)
		if p.matchKeyword("PRIMARY") {
			if !p.matchKeyword("KEY") {
				err = fmt.Errorf("expected KEY")
				return
			}
			if q := p.next(); q.kind != "sym" || q.val != "(" {
				err = fmt.Errorf("expected (")
				return
			}
			pk, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			pkName = pk
			pkNameSet = true
			if p.matchSym(")") {
				break
			}
			if q := p.next(); q.kind != "sym" || q.val != "," {
				err = fmt.Errorf("expected ,")
				return
			}
			continue
		}
		fname, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		typeTok := p.next()
		if typeTok.kind != "kw" && typeTok.kind != "ident" {
			err = fmt.Errorf("expected type after %s", fname)
			return
		}
		ftype, e := parseFieldType(typeTok.val)
		if e != nil {
			err = e
			return
		}
		flen := 0
		if ftype == TypeVarchar {
			flen = 255
		}
		if q := p.next(); q.kind == "sym" && q.val == "(" {
			num := p.next()
			if num.kind != "num" {
				err = fmt.Errorf("expected length")
				return
			}
			flen, _ = strconv.Atoi(num.val)
			if q := p.next(); q.kind != "sym" || q.val != ")" {
				err = fmt.Errorf("expected )")
				return
			}
		} else {
			p.i-- // 回退已读符号
		}
		// 行内 PRIMARY KEY
		if p.peek().kind == "kw" && p.peek().upper == "PRIMARY" {
			p.next()
			p.next() // KEY
			if !pkNameSet {
				pkName = fname
				pkNameSet = true
			}
		}
		fields = append(fields, Field{Name: fname, Type: ftype, Len: flen})
		if p.matchSym(")") {
			break
		}
		if q := p.next(); q.kind != "sym" || q.val != "," {
			err = fmt.Errorf("expected , or )")
			return
		}
	}
	if !pkNameSet {
		err = fmt.Errorf("table must have a PRIMARY KEY")
		return
	}
	meta := &TableMeta{
		Name:    name,
		PK:      pkName,
		Fields:  fields,
		Indexes: map[string]string{},
	}
	db.mu.Lock()
	if _, ok := db.tables.Load(name); ok {
		db.mu.Unlock()
		err = fmt.Errorf("table %s already exists", name)
		return
	}
	table := NewTable(meta, db.walFile)
	db.tables.Store(name, table)
	db.tablesCount.Add(1)
	db.catalog[name] = meta
	we := db.writeCatalog(meta)
	db.mu.Unlock()
	if we != nil {
		err = we
		return
	}
	affected = 1
	rawMsg = fmt.Sprintf("table %s created", name)
	return
}

func (db *DB) parseDropTable(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // DROP
	if !p.matchKeyword("TABLE") {
		err = fmt.Errorf("expected TABLE")
		return
	}
	ifExists := p.matchKeyword("IF")
	ifExists = p.matchKeyword("EXISTS") || ifExists
	name, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	name = db.qualifyTable(name)
	db.mu.Lock()
	_, ok := db.catalog[name]
	db.mu.Unlock()
	if !ok {
		if ifExists {
			// IF EXISTS 且表不存在：静默成功
			rawMsg = "Query OK"
			return
		}
		err = fmt.Errorf("table not found: %s", name)
		return
	}
	if err := db.dropTableByName(name); err != nil {
		return nil, nil, 0, "", err
	}
	affected = 1
	rawMsg = fmt.Sprintf("table %s dropped", name)
	return
}

// dropTableByName 删除单个表（写 WAL + 从内存移除）。
func (db *DB) dropTableByName(name string) error {
	db.mu.Lock()
	meta, ok := db.catalog[name]
	if !ok {
		db.mu.Unlock()
		return nil
	}
	// 写 DROP 到 WAL：v1 紧凑格式 [100][subop=2][varint tableID][varint nameLen][name][CRC]
	buf := make([]byte, 0, 16+len(meta.Name))
	buf = append(buf, 100, 2)
	buf = appendVarUint(buf, uint64(walRegRegister(meta.Name)))
	buf = appendVarLenStr(buf, meta.Name)
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf); err != nil {
		db.mu.Unlock()
		return err
	}
	db.tables.Delete(name)
	db.tablesCount.Add(-1)
	delete(db.catalog, name)
	walRegUnregister(name)
	db.mu.Unlock()
	return db.flushWAL()
}

// parseAlterTable 处理 ALTER TABLE：
//   - ALTER TABLE t ADD COLUMN name TYPE [LENGTH]  （追加列，向后兼容已编码行）
//   - ALTER TABLE t RENAME TO new
// 仅支持向表末尾追加列与重命名；不改变既有字段顺序（避免破坏既有 WAL 行编码）。
func (db *DB) parseAlterTable(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // ALTER
	if !p.matchKeyword("TABLE") {
		err = fmt.Errorf("expected TABLE")
		return
	}
	name, e := p.parseIdent()
	if e != nil {
		err = e
		return
	}
	name = db.qualifyTable(name)
	t := db.getTable(name)
	if t == nil {
		err = fmt.Errorf("table not found: %s", name)
		return
	}
	if p.matchKeyword("ADD") {
		if !p.matchKeyword("COLUMN") {
			err = fmt.Errorf("expected COLUMN")
			return
		}
		fname, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		typeTok := p.next()
		ftype, e := parseFieldType(typeTok.val)
		if e != nil {
			err = e
			return
		}
		flen := 0
		if ftype == TypeVarchar {
			flen = 255
		}
		if q := p.next(); q.kind == "sym" && q.val == "(" {
			num := p.next()
			if num.kind != "num" {
				err = fmt.Errorf("expected length")
				return
			}
			flen, _ = strconv.Atoi(num.val)
			if q := p.next(); q.kind != "sym" || q.val != ")" {
				err = fmt.Errorf("expected )")
				return
			}
		} else {
			p.i-- // 回退已读符号
		}
		// 追加新字段（保持既有字段序），重写 catalog
		db.mu.Lock()
		for _, f := range t.meta.Fields {
			if f.Name == fname {
				db.mu.Unlock()
				err = fmt.Errorf("column %s already exists", fname)
				return
			}
		}
		t.meta.Fields = append(t.meta.Fields, Field{Name: fname, Type: ftype, Len: flen})
		db.catalog[name] = t.meta
		we := db.writeCatalog(t.meta)
		db.mu.Unlock()
		if we != nil {
			err = we
			return
		}
		rawMsg = fmt.Sprintf("column %s added to table %s", fname, name)
		affected = 1
		return
	}
	if p.matchKeyword("RENAME") {
		if !p.matchKeyword("TO") {
			err = fmt.Errorf("expected TO")
			return
		}
		newname, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		if strings.Contains(newname, ".") {
			// 仅允许同库重命名；跨库重命名不做特殊处理
		} else {
			cur := db.getCurDB()
			if cur != "" && cur != "tsumugi" {
				newname = cur + "." + newname
			}
		}
		if db.getTable(newname) != nil {
			err = fmt.Errorf("table %s already exists", newname)
			return
		}
		// 表重命名：写一条 catalog RENAME 记录（同 ID 改名，保留行数据），再更新内存。
		db.mu.Lock()
		oldName := t.meta.Name
		oldID := walRegRegister(oldName)
		t.meta.Name = newname
		db.tables.Delete(oldName)
		db.tables.Store(newname, t)
		delete(db.catalog, oldName)
		db.catalog[newname] = t.meta
		walRegRename(oldName, newname, oldID)
		we := db.writeCatalogRename(oldID, newname)
		db.mu.Unlock()
		if we != nil {
			err = we
			return
		}
		rawMsg = fmt.Sprintf("table %s renamed to %s", oldName, newname)
		affected = 1
		return
	}
	err = fmt.Errorf("unsupported ALTER TABLE operation")
	return
}

// ==================== SQL 辅助 ====================

// likeMatch SQL LIKE 匹配：% 任意多字符，_ 单字符，其余大小写不敏感字面匹配。
func likeMatch(s, pattern string) bool {
	return likeMatchFold(strings.ToLower(s), strings.ToLower(pattern), 0, 0)
}

func likeMatchFold(s, p string, i, j int) bool {
	for j < len(p) {
		switch p[j] {
		case '%':
			for k := i; k <= len(s); k++ {
				if likeMatchFold(s, p, k, j+1) {
					return true
				}
			}
			return false
		case '_':
			if i >= len(s) {
				return false
			}
			i++
			j++
		default:
			if i >= len(s) || s[i] != p[j] {
				return false
			}
			i++
			j++
		}
	}
	return i == len(s)
}

// sortRowsByFirst 按首列字符串升序排序。
func sortRowsByFirst(rows [][]interface{}) {
	sort.Slice(rows, func(a, b int) bool {
		sa, aok := rows[a][0].(string)
		sb, bok := rows[b][0].(string)
		if !aok || !bok {
			return false
		}
		return sa < sb
	})
}