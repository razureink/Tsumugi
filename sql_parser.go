package main

import (
	"fmt"
	"time"
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

// putSQLTokens 归还 token 切片到池；超大切片（如 16MB 长查询产生的百万级
// token）不入池，避免池长期钉住大块内存。
func putSQLTokens(toks []sqlToken) {
	if cap(toks) <= 4096 {
		sqlTokenPool.Put(toks[:0])
	}
}

// interfaceSlicePool 复用 []interface{} 切片，用于行构建等临时场景。
var interfaceSlicePool = sync.Pool{
	New: func() interface{} { return make([]interface{}, 0, 8) },
}

// tokenizeSQL SQL 分词：关键字/标识符（含反引号引用）、单/双引号字符串
// （支持反斜杠转义与引号 doubling）、整数与浮点、符号、注释（-- # /* */）。
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
		case c == '-' && i+2 <= n && sql[i:i+2] == "--" && (i+2 >= n || sql[i+2] == ' ' || sql[i+2] == '\t'):
			// 行注释（MySQL 要求 -- 后跟空白）
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '#':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+2 <= n && sql[i:i+2] == "/*":
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				putSQLTokens(toks)
				return nil, fmt.Errorf("unterminated comment")
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			val, ni, err := scanQuoted(sql, i)
			if err != nil {
				putSQLTokens(toks)
				return nil, err
			}
			toks = append(toks, sqlToken{kind: "str", val: val, pos: i})
			i = ni
		case c == '`':
			// 反引号引用标识符：`` doubling 表示字面反引号（MySQL 语义）。
			j := i + 1
			escaped := false
			for j < n {
				if sql[j] == '`' {
					if j+1 < n && sql[j+1] == '`' {
						escaped = true
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= n {
				putSQLTokens(toks)
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			var val string
			if !escaped {
				val = sql[i+1 : j]
			} else {
				var sb strings.Builder
				for k := i + 1; k < j; k++ {
					if sql[k] == '`' && k+1 < j && sql[k+1] == '`' {
						sb.WriteByte('`')
						k++
						continue
					}
					sb.WriteByte(sql[k])
				}
				val = sb.String()
			}
			// 引用标识符永不作为关键字
			toks = append(toks, sqlToken{kind: "ident", val: val, pos: i})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '-' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9') || (c == '.' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9'):
			j := i
			if c == '-' {
				j++
			}
			isFloat := c == '.'
			for j < n && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '.') {
				if sql[j] == '.' {
					// "db.table" 的点不属于数字：点后必须还是数字且当前数字段未含点
					if isFloat || j+1 >= n || sql[j+1] < '0' || sql[j+1] > '9' {
						break
					}
					isFloat = true
				}
				j++
			}
			// 科学计数法 1e5 / 1.5E-3
			if j < n && (sql[j] == 'e' || sql[j] == 'E') {
				k := j + 1
				if k < n && (sql[k] == '+' || sql[k] == '-') {
					k++
				}
				if k < n && sql[k] >= '0' && sql[k] <= '9' {
					for k < n && sql[k] >= '0' && sql[k] <= '9' {
						k++
					}
					isFloat = true
					j = k
				}
			}
			toks = append(toks, sqlToken{kind: "num", val: sql[i:j], pos: i})
			i = j
		case isIdentChar(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			word := sql[i:j]
			// 关键字：内部化 canonical 大写（零分配）；普通标识符不再预计算
			// ToUpper（旧实现每个含小写字母的 token 一次堆分配，是分词热路径
			// 的主要分配源）。ident 的大小写不敏感比较由消费方 EqualFold 完成。
			kind := "ident"
			var upper string
			if c := keywordCanonical(word); c != "" {
				kind, upper = "kw", c
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

// scanQuoted 扫描单/双引号字符串，处理反斜杠转义与
// 引号 doubling（'' 或 ""），返回解码后的值与结束后的下一个下标。
func scanQuoted(sql string, i int) (string, int, error) {
	quote := sql[i]
	n := len(sql)
	j := i + 1
	var sb strings.Builder
	simple := true
	for j < n {
		ch := sql[j]
		if ch == '\\' && j+1 < n {
			// MySQL 默认模式：反斜杠转义（\n \t \r \0 \' \" \\ \b \Z）
			simple = false
			nc := sql[j+1]
			switch nc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '0':
				sb.WriteByte(0)
			case 'b':
				sb.WriteByte(8)
			case 'Z':
				sb.WriteByte(26)
			default:
				sb.WriteByte(nc)
			}
			j += 2
			continue
		}
		if ch == quote {
			if j+1 < n && sql[j+1] == quote {
				simple = false
				sb.WriteByte(quote)
				j += 2
				continue
			}
			break
		}
		sb.WriteByte(ch)
		j++
	}
	if j >= n {
		return "", 0, fmt.Errorf("unterminated string literal")
	}
	if simple {
		return sql[i+1 : j], j + 1, nil
	}
	return sb.String(), j + 1, nil
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

// sqlKeywords 保留关键字表（canonical 大写形式，单一来源）。
var sqlKeywords = []string{
	"AND",
	"AS",
	"ASC",
	"BETWEEN",
	"BIGINT",
	"BOOL",
	"BOOLEAN",
	"BY",
	"CHAR",
	"CREATE",
	"DELETE",
	"DESC",
	"DESCRIBE",
	"DROP",
	"FALSE",
	"FROM",
	"GROUP",
	"HAVING",
	"INSERT",
	"INT",
	"INTEGER",
	"INTO",
	"IS",
	"KEY",
	"LIKE",
	"LIMIT",
	"NOT",
	"NULL",
	"OR",
	"ORDER",
	"PRIMARY",
	"SELECT",
	"SET",
	"SHOW",
	"STRING",
	"TABLE",
	"TABLES",
	"TEXT",
	"TRUE",
	"UPDATE",
	"VALUES",
	"VARCHAR",
	"WHERE",
}

// kwByLen 按长度分桶的关键字表：keywordCanonical 对每个词只做同长度词的
// EqualFold 扫描（通常个位数候选），零分配。
var kwByLen [16][]string

func init() {
	for _, k := range sqlKeywords {
		if len(k) < len(kwByLen) {
			kwByLen[len(k)] = append(kwByLen[len(k)], k)
		}
	}
}

// keywordCanonical 若 word 是关键字（大小写不敏感），返回内部化的大写形式
// （零分配）；否则返回空串。取代旧的 isKeyword + strings.ToUpper 组合——
// 后者对每个含小写字母的 token 都产生一次堆分配。
func keywordCanonical(word string) string {
	if len(word) >= len(kwByLen) {
		return ""
	}
	for _, k := range kwByLen[len(word)] {
		if strings.EqualFold(k, word) {
			return k
		}
	}
	return ""
}

type sqlParser struct {
	toks     []sqlToken
	i        int
	params   []interface{} // 非 nil 时表示处于参数绑定模式（prepared execute）
	paramIdx int           // 已消费的参数下标
	txnID    uint64        // 当前事务 ID（0 表示自动提交）
	schema   string        // 连接级默认数据库（MySQL 会话）；空表示沿用全局 curDB（Web 控制台）
}

// curSchema 返回本语句的默认数据库：连接级 schema 优先，其次全局 curDB。
func (p *sqlParser) curSchema(db *DB) string {
	if p.schema != "" {
		return p.schema
	}
	return db.getCurDB()
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

// matchKeyword 匹配关键字/标识符，大小写不敏感。
// kw token 的 upper 已内部化（== 快速路径）；ident（含 SHOW COLUMNS 等
// 非关键字字面量）走 EqualFold——ident.upper 已不再预计算。
func (p *sqlParser) matchKeyword(kw string) bool {
	t := p.peek()
	if t.kind == "kw" {
		if t.upper == kw {
			p.next()
			return true
		}
		return false
	}
	if t.kind == "ident" && strings.EqualFold(t.val, kw) {
		p.next()
		return true
	}
	return false
}

// is 大小写不敏感地比较 token 与给定词（关键字与标识符均适用）。
func (t sqlToken) is(word string) bool {
	if t.upper != "" {
		return t.upper == word
	}
	return strings.EqualFold(t.val, word)
}

// stmtWords 保留字表之外的语句级语法词：它们不在 sqlKeywords 中（避免侵占
// 标识符命名空间），但作为语句首词/子句词参与分派，需大小写不敏感归一。
var stmtWords = []string{"ALTER", "SETS", "DATABASE", "USE"}

// canonical 返回 token 的规整大写形式：关键字用内部化 upper（零分配），
// 其余按 stmtWords 大小写不敏感解析。
func (t sqlToken) canonical() string {
	if t.upper != "" {
		return t.upper
	}
	if t.kind != "ident" {
		return ""
	}
	for _, w := range stmtWords {
		if strings.EqualFold(t.val, w) {
			return w
		}
	}
	return ""
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
	return db.runSQLSchema(sql, params, txnID, "")
}

// runSQLSchema 执行 SQL 并携带连接级默认数据库（MySQL 会话的 USE 作用域）。
func (db *DB) runSQLSchema(sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	toks, err := tokenizeSQL(sql)
	if err != nil {
		return
	}
	defer putSQLTokens(toks)
	return db.runSQLTokens(toks, sql, params, txnID, schema)
}

// runSQLTokens 使用调用方提供的已分词 tokens 执行（prepared 复用，避免每次 EXECUTE 重新分词）。
func (db *DB) runSQLTokens(toks []sqlToken, sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p := &sqlParser{toks: toks, params: params, txnID: txnID, schema: schema}
	// 去除开头的空/分号
	for p.peek().kind == "sym" && p.peek().val == ";" {
		p.next()
	}
	first := p.peek()
	if first.kind == "eof" {
		err = fmt.Errorf("empty query")
		return
	}
	switch first.canonical() {
	case "SHOW":
		if !p.matchKeyword("SHOW") {
			break
		}
		// SHOW FULL COLUMNS / SHOW COLUMNS FROM tbl [FROM db] [LIKE '..']
		p.matchKeyword("FULL")
		if p.peek().is("COLUMNS") {
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
			columns, rows, err = db.describeTable(tableRef{p.readOptionalDBName(p.curSchema(db)), tbl})
			return
		}
		// SHOW TABLE STATUS FROM db [LIKE '..']
		if p.matchKeyword("TABLE") && p.matchKeyword("STATUS") {
			dbName := p.readOptionalDBName(p.curSchema(db))
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
			columns, rows, err = db.showIndex(tableRef{p.readOptionalDBName(p.curSchema(db)), tbl})
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
			if g := mysqlCfg.defaultUser(); g != "" {
				grantees = g
			}
			columns = []string{"Grants for " + grantees}
			for _, u := range mysqlCfg.get().Users {
				rows = append(rows, []interface{}{"GRANT USAGE ON *.* TO `" + u.User + "`@`" + u.Host + "`"})
			}
			return
		}
		// SHOW ENGINES
		if p.matchKeyword("ENGINES") {
			columns, rows, err = db.queryInfoSchema("ENGINES", nil, nil, nil)
			return
		}
		// SHOW PLUGINS
		if p.matchKeyword("PLUGINS") {
			columns, rows, err = db.queryInfoSchema("PLUGINS", nil, nil, nil)
			return
		}
		// SHOW CHARACTER SET / SHOW CHARSET
		if p.matchKeyword("CHARACTER") {
			p.matchKeyword("SET")
			columns, rows, err = db.queryInfoSchema("CHARACTER_SETS", nil, nil, nil)
			return
		}
		if p.matchKeyword("CHARSET") {
			columns, rows, err = db.queryInfoSchema("CHARACTER_SETS", nil, nil, nil)
			return
		}
		// SHOW COLLATION [LIKE '..']
		if p.matchKeyword("COLLATION") {
			if p.matchKeyword("LIKE") {
				p.next()
			}
			columns, rows, err = db.queryInfoSchema("COLLATIONS", nil, nil, nil)
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
			cur := p.curSchema(db)
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
		if p.i+1 < len(p.toks) && p.toks[p.i+1].is("DATABASE") {
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
		if p.i+1 < len(p.toks) && p.toks[p.i+1].is("DATABASE") {
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
		// 物理表名带 db. 前缀：裸名先精确匹配，再按当前连接库限定
		qual := db.qualifyTableIn(nameTok.val, p.curSchema(db))
		for _, tb := range tables {
			if tb["name"] == nameTok.val || tb["name"] == qual {
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
	defer putSQLTokens(toks)
	return countSQLTokens(toks)
}

func (p *sqlParser) parseIdent() (string, error) {
	return p.readName(false)
}

// readTableIdent 读取表名/库表名标识符；关键字也可作表名（如 information_schema.TABLES、mysql.KILL）。
func (p *sqlParser) readTableIdent() (string, error) {
	return p.readName(true)
}

// readName 读取标识符（allowKeyword 时允许关键字）；字符串字面量始终允许。
func (p *sqlParser) readName(allowKeyword bool) (string, error) {
	t := p.next()
	ok := t.kind == "ident" || t.kind == "str" || (allowKeyword && t.kind == "kw")
	if !ok {
		return "", fmt.Errorf("expected identifier, got %q", t.val)
	}
	return t.val, nil
}

// readQualifiedTable 读取表名并消费 "db.table" 形式的点号拼接。
func (p *sqlParser) readQualifiedTable() (string, error) {
	name, e := p.readTableIdent()
	if e != nil {
		return "", e
	}
	for p.peek().kind == "sym" && p.peek().val == "." {
		p.next()
		part, pe := p.readTableIdent()
		if pe != nil {
			return "", pe
		}
		name += "." + part
	}
	return name, nil
}

// readOptionalDBName 读取可选的 "FROM db" 子句；缺省或后续非名称时返回 cur。
func (p *sqlParser) readOptionalDBName(cur string) string {
	if !p.matchKeyword("FROM") {
		return cur
	}
	if d := p.next(); d.kind == "ident" || d.kind == "str" {
		return d.val
	}
	return cur
}

// parseEqConds 解析纯等值 WHERE（仅支持 "=" 与 AND，phpMyAdmin 惯例），
// 返回字段名 -> 值映射；无 WHERE 时返回空映射。
func (p *sqlParser) parseEqConds() (map[string]interface{}, error) {
	conds := map[string]interface{}{}
	if !p.matchKeyword("WHERE") {
		return conds, nil
	}
	for {
		f, e := p.parseIdent()
		if e != nil {
			return nil, e
		}
		op := p.next()
		if op.kind != "sym" || op.val != "=" {
			return nil, fmt.Errorf("unsupported condition in COUNT")
		}
		v, e := p.parseValue()
		if e != nil {
			return nil, e
		}
		conds[f] = v
		if !p.matchKeyword("AND") {
			break
		}
	}
	return conds, nil
}

// srcColumn 按 colSrc 别名映射返回输出列名对应的源列名；无映射时原样返回。
func srcColumn(colSrc map[string]string, colName string) string {
	if colSrc != nil {
		if s, ok := colSrc[strings.ToLower(colName)]; ok {
			return s
		}
	}
	return colName
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
		if strings.ContainsAny(t.val, ".eE") {
			// 浮点字面量按 float64 返回；列类型为 INT 时由写入路径取整
			v, err := strconv.ParseFloat(t.val, 64)
			if err != nil {
				return nil, err
			}
			return v, nil
		}
		v, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "str":
		return t.val, nil
	case "kw":
		switch t.upper {
		case "TRUE":
			return true, nil
		case "FALSE":
			return false, nil
		case "NULL":
			return nil, nil
		}
	}
	return nil, fmt.Errorf("unexpected value %q", t.val)
}

// rawCond 一条原始比较条件。
type rawCond struct {
	field string
	op    string // "=" ">" "<" ">=" "<=" "<>" "!=" "BETWEEN" "LIKE" "ISNULL" "ISNOTNULL"
	val   interface{}
	val2  interface{} // BETWEEN 上限
}

// andGroup 以 AND 连接的条件组；orGroups 各组之间以 OR 连接。
// 无括号 SQL 的标准优先级：AND 高于 OR，因此按 OR 分割成若干 AND 组。
type andGroup []*rawCond

func (db *DB) parseSelect(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // SELECT
	// 无 FROM 的标量查询（SELECT @@version, SELECT VERSION(), SELECT NOW() 等）
	if c, r, e := db.queryScalarSelect(p); r != nil {
		columns, rows, err = c, r, e
		return
	}
	// COUNT 聚合：SELECT COUNT(*) FROM tbl [WHERE ..] / SELECT COUNT(1) 等
	isCount := p.peek().is("COUNT") && p.toks[p.i+1].kind == "sym" && p.toks[p.i+1].val == "("
	if isCount {
		p.next() // COUNT
		p.next() // (
		for {
			t := p.next()
			if t.kind == "sym" && t.val == ")" {
				break
			}
			if t.kind == "eof" {
				err = fmt.Errorf("expected ) in COUNT")
				return
			}
		}
		if !p.matchKeyword("FROM") {
			err = fmt.Errorf("expected FROM")
			return
		}
		columns, rows, affected, rawMsg, err = db.countSelect(p)
		return
	}
	var selectCols []string
	colSrc := map[string]string{} // 输出列名 -> 源列名（AS 别名映射）
	if pt := p.peek(); pt.kind == "sym" && pt.val == "*" {
		p.next()
		selectCols = nil // 表示所有列
		colSrc = nil
	} else {
		readCol := func() (string, error) {
			ct := p.peek()
			if ct.kind != "ident" && ct.kind != "kw" && ct.kind != "str" {
				return "", fmt.Errorf("expected column name, got %q", ct.val)
			}
			p.next()
			return ct.val, nil
		}
		for {
			field, e := readCol()
			if e != nil {
				err = e
				return
			}
			src := field
			colName := field
			if p.peek().kind == "sym" && p.peek().val == "." {
				p.next()
				part, pe := readCol()
				if pe != nil {
					err = pe
					return
				}
				src = field + "." + part
				colName = src
			}
			// phpMyAdmin 常用 SELECT col AS alias 形式
			if p.matchKeyword("AS") {
				alias, ae := readCol()
				if ae != nil {
					err = ae
					return
				}
				colName = alias
			}
			selectCols = append(selectCols, colName)
			colSrc[strings.ToLower(colName)] = src
			if !p.matchSym(",") {
				break
			}
		}
	}
	if !p.matchKeyword("FROM") {
		err = fmt.Errorf("expected FROM")
		return
	}
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	// 非 mysql.* 且无前缀时按当前数据库解析（仅非默认库加前缀）
	if !strings.HasPrefix(strings.ToLower(tableName), "mysql.") {
		tableName = db.qualifyTableIn(tableName, p.curSchema(db))
	}
	// WHERE：解析为 OR 连接的 AND 条件组（支持 = / 范围 / BETWEEN / LIKE / IS [NOT] NULL）
	var orGroups []andGroup
	if p.matchKeyword("WHERE") {
		orGroups = make([]andGroup, 1)
		for {
			field, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			cur := &orGroups[len(orGroups)-1]
			addCond := func(c *rawCond) {
				*cur = append(*cur, c)
			}
			if p.matchKeyword("IS") {
				neg := p.matchKeyword("NOT")
				if !p.matchKeyword("NULL") {
					err = fmt.Errorf("expected NULL after IS")
					return
				}
				if neg {
					addCond(&rawCond{field: field, op: "ISNOTNULL"})
				} else {
					addCond(&rawCond{field: field, op: "ISNULL"})
				}
			} else {
				op := p.next()
				// BETWEEN / LIKE 是关键字(kw)，其余比较符是符号(sym)
				isKwOp := (op.kind == "kw" || op.kind == "ident") && (op.upper == "BETWEEN" || op.upper == "LIKE")
				if op.kind != "sym" && !isKwOp {
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
					addCond(&rawCond{field: field, op: strings.ToUpper(op.val), val: val})
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
					addCond(&rawCond{field: field, op: "BETWEEN", val: lo, val2: hi})
				case "LIKE":
					pat, e := p.parseValue()
					if e != nil {
						err = e
						return
					}
					addCond(&rawCond{field: field, op: "LIKE", val: pat})
				default:
					err = fmt.Errorf("unsupported comparison operator: %s", op.val)
					return
				}
			}
			if p.matchKeyword("AND") {
				continue
			}
			if p.matchKeyword("OR") {
				orGroups = append(orGroups, andGroup{})
				continue
			}
			break
		}
	}
	// ORDER BY：解析并真正排序（不再静默丢弃）
	type orderBy struct {
		field string
		desc  bool
	}
	var orderBys []orderBy
	for {
		if p.matchKeyword("ORDER") {
			if !p.matchKeyword("BY") {
				err = fmt.Errorf("expected BY after ORDER")
				return
			}
			for {
				f, e := p.parseIdent()
				if e != nil {
					err = e
					return
				}
				ob := orderBy{field: f}
				if p.matchKeyword("DESC") {
					ob.desc = true
				} else {
					p.matchKeyword("ASC")
				}
				orderBys = append(orderBys, ob)
				if !p.matchSym(",") {
					break
				}
			}
			continue
		}
		// GROUP BY / HAVING 无聚合引擎支撑，宁可报错也不再静默返回未分组结果
		if p.matchKeyword("GROUP") {
			err = fmt.Errorf("GROUP BY is not supported")
			return
		}
		if p.matchKeyword("HAVING") {
			err = fmt.Errorf("HAVING is not supported")
			return
		}
		break
	}
	limit := -1
	offset := int64(0)
	if p.matchKeyword("LIMIT") {
		num := p.next()
		if num.kind != "num" {
			err = fmt.Errorf("expected number after LIMIT")
			return
		}
		v, e := strconv.ParseInt(num.val, 10, 64)
		if e != nil || v < 0 {
			err = fmt.Errorf("invalid LIMIT value")
			return
		}
		if p.matchSym(",") {
			// MySQL 形式 LIMIT offset, count（phpMyAdmin 浏览固定发 LIMIT 0, N）
			offset = v
			num2 := p.next()
			if num2.kind != "num" {
				err = fmt.Errorf("expected number after LIMIT offset,")
				return
			}
			v2, e2 := strconv.ParseInt(num2.val, 10, 64)
			if e2 != nil || v2 < 0 {
				err = fmt.Errorf("invalid LIMIT value")
				return
			}
			limit = int(v2)
		} else if p.matchKeyword("OFFSET") {
			num2 := p.next()
			if num2.kind != "num" {
				err = fmt.Errorf("expected number after OFFSET")
				return
			}
			off, e2 := strconv.ParseInt(num2.val, 10, 64)
			if e2 != nil || off < 0 {
				err = fmt.Errorf("invalid OFFSET value")
				return
			}
			offset = off
			limit = int(v)
		} else {
			limit = int(v)
		}
	}
	// 语句尾部必须耗尽：剩余 token 意味着解析器没有覆盖该语法，
	// 报错而不是静默忽略（此前 OR 条件 / 未支持子句会被无声丢弃）。
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	// mysql.* 虚拟系统表
	if strings.HasPrefix(strings.ToLower(tableName), "mysql.") {
		mt := strings.ToLower(strings.TrimPrefix(tableName, "mysql."))
		conds := condsFromGroups(orGroups)
		if conds == nil && len(orGroups) > 1 {
			err = fmt.Errorf("OR in WHERE is not supported for system tables")
			return
		}
		return db.queryMysqlTable(mt, conds, selectCols, colSrc)
	}
	// information_schema.* 虚拟系统表
	if strings.HasPrefix(strings.ToLower(tableName), "information_schema.") {
		it := strings.ToLower(strings.TrimPrefix(tableName, "information_schema."))
		conds := condsFromGroups(orGroups)
		if conds == nil && len(orGroups) > 1 {
			err = fmt.Errorf("OR in WHERE is not supported for system tables")
			return
		}
		columns, rows, err = db.queryInfoSchema(it, conds, selectCols, colSrc)
		return
	}
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	// 单一 AND 组且无 OR 时走索引/范围下推快速路径；含 OR 时退化为全表谓词过滤
	hasOR := len(orGroups) > 1
	// 主键点查快速路径：单一 `pk = 整数常量` 条件且无 ORDER BY / OFFSET 时，
	// 跳过条件 map 构建与 SelectR 的通用分发，直接点查后按列投影。
	pkPoint := false
	var pkPointVal int64
	if len(orGroups) == 1 && len(orGroups[0]) == 1 && len(orderBys) == 0 && offset == 0 {
		c := orGroups[0][0]
		if c.op == "=" && strings.EqualFold(c.field, t.Meta().PK) {
			if v, ok := c.val.(int64); ok {
				pkPointVal, pkPoint = v, true
			}
		}
	}
	conds := map[string]interface{}{}
	var minKey, maxKey *int64
	var filters []RangeCond
	if len(orGroups) == 1 && !pkPoint {
		for _, c := range orGroups[0] {
			if c.op == "=" {
				conds[c.field] = c.val
				continue
			}
			if strings.EqualFold(c.field, t.Meta().PK) {
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
			if c.op == "LIKE" || c.op == "ISNULL" || c.op == "ISNOTNULL" {
				// 不可下推的条件走全表谓词过滤
				hasOR = true
				continue
			}
			filters = append(filters, RangeCond{Field: c.field, Op: rangeOp(c.op), Val: c.val, Val2: c.val2})
		}
	}
	// 列投影准备：SELECT * 返回全部列；否则只返回指定的列（AS 别名映射到源列）
	cols := t.Meta().Fields
	outNames := []string(nil)
	if selectCols != nil {
		cols = nil
		for _, sc := range selectCols {
			src := sc
			if colSrc != nil {
				if s, ok := colSrc[strings.ToLower(sc)]; ok {
					src = s
				}
			}
			for _, f := range t.Meta().Fields {
				if strings.EqualFold(src, f.Name) {
					cols = append(cols, f)
					outNames = append(outNames, sc)
					break
				}
			}
		}
		if len(cols) == 0 {
			err = fmt.Errorf("no matching columns")
			return
		}
	}
	if outNames == nil {
		outNames = make([]string, 0, len(cols))
		for _, f := range cols {
			outNames = append(outNames, f.Name)
		}
	}
	columns = outNames
	// ORDER BY / OFFSET 需要完整结果集后再裁剪，扫描阶段不能提前 limit
	scanLimit := limit
	if hasOR || len(orderBys) > 0 || offset > 0 {
		scanLimit = -1
	}
	// 主键点查：树内直接命中 + TTL 检查 + 单行投影，避开通用扫描路径
	if pkPoint {
		if limit == 0 {
			rows = nil
			return
		}
		rowData, found := t.pkTree.Get(pkPointVal)
		if !found {
			rows = nil
			return
		}
		if e := decodeExpireAt(rowData); e > 0 && time.Now().UnixNano() > e {
			t.pkTree.Delete(pkPointVal)
			rows = nil
			return
		}
		r, _, _ := decodeRow(t.Meta(), rowData)
		row := make([]interface{}, 0, len(cols))
		for _, f := range cols {
			row = append(row, r[f.Name])
		}
		rows = [][]interface{}{row}
		return
	}
	// 扫描 + 过滤 + 投影
	if hasOR {
		// 全表扫描 + 逐行谓词求值（OR 或 LIKE / IS NULL 等不可下推条件）
		var expired []int64
		now := time.Now().UnixNano()
		matched := make([]map[string]interface{}, 0, 64)
		t.pkTree.scanRangeStop(minKey, maxKey, func(key int64, value []byte) bool {
			if e := decodeExpireAt(value); e > 0 && now > e {
				expired = append(expired, key)
				return true
			}
			row, _, _ := decodeRow(t.Meta(), value)
			if rowMatchGroups(row, orGroups) {
				matched = append(matched, row)
			}
			return true
		})
		for _, k := range expired {
			t.pkTree.Delete(k)
		}
		rows = make([][]interface{}, 0, len(matched))
		for _, r := range matched {
			row := make([]interface{}, 0, len(cols))
			for _, f := range cols {
				row = append(row, r[f.Name])
			}
			rows = append(rows, row)
		}
	} else if len(conds) == 0 && len(filters) == 0 {
		// 无 WHERE 条件的快速路径：直接解码为 []interface{}，跳过每行 map 分配
		flat := t.selectRowsFlat(minKey, maxKey, scanLimit)
		if selectCols == nil {
			rows = flat
		} else {
			// 列投影：flat 行按 meta.Fields 顺序排列，按下标取值
			idx := make([]int, 0, len(cols))
			for _, f := range cols {
				for i, mf := range t.Meta().Fields {
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
			}
		}
	} else {
		res, e := t.SelectR(conds, filters, minKey, maxKey, scanLimit)
		if e != nil {
			err = e
			return
		}
		rows = make([][]interface{}, 0, len(res))
		for _, r := range res {
			row := make([]interface{}, 0, len(cols))
			for _, f := range cols {
				row = append(row, r[f.Name])
			}
			rows = append(rows, row)
		}
	}
	// ORDER BY：按输出列排序（找不到列则显式报错）
	if len(orderBys) > 0 {
		idxs := make([]int, 0, len(orderBys))
		for _, ob := range orderBys {
			found := -1
			for i, name := range outNames {
				if strings.EqualFold(name, ob.field) {
					found = i
					break
				}
			}
			if found < 0 && colSrc != nil {
				if src, ok := colSrc[strings.ToLower(ob.field)]; ok {
					for i, name := range outNames {
						if strings.EqualFold(name, src) {
							found = i
							break
						}
					}
				}
			}
			if found < 0 {
				err = fmt.Errorf("unknown column %q in ORDER BY", ob.field)
				return
			}
			idxs = append(idxs, found)
		}
		sort.SliceStable(rows, func(a, b int) bool {
			for k, ob := range orderBys {
				i := idxs[k]
				c := cmpCell(rows[a][i], rows[b][i])
				if ob.desc {
					c = -c
				}
				if c != 0 {
					return c < 0
				}
			}
			return false
		})
	}
	// OFFSET / LIMIT 截断（扫描阶段未提前限制时在此裁剪）
	if offset > 0 {
		if int64(len(rows)) > offset {
			rows = rows[offset:]
		} else {
			rows = nil
		}
	}
	if limit >= 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return
}

// expectEnd 断言语句已解析到结尾（允许尾随分号）。剩余 token 说明存在
// 解析器不支持的语法，必须显式报错而不是静默丢弃。
func (p *sqlParser) expectEnd() error {
	for p.peek().kind == "sym" && p.peek().val == ";" {
		p.next()
	}
	if p.peek().kind != "eof" {
		return fmt.Errorf("unexpected %q after statement (unsupported syntax is rejected, not ignored)", p.peek().val)
	}
	return nil
}

// condsFromGroups 单一 AND 组时提取等值条件供系统表过滤；含 OR 时返回 nil。
func condsFromGroups(orGroups []andGroup) map[string]interface{} {
	if len(orGroups) != 1 {
		return nil
	}
	conds := map[string]interface{}{}
	for _, c := range orGroups[0] {
		if c.op == "=" {
			conds[c.field] = c.val
		}
	}
	return conds
}

// cmpCell 结果集排序比较：数值按大小、字符串按字典序、类型不可比视为相等
//（稳定排序保持原顺序），nil 视为最小。
func cmpCell(a, b interface{}) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	av, aok := toNum(a)
	bv, bok := toNum(b)
	if aok && bok {
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
	}
	as, aokS := a.(string)
	bs, bokS := b.(string)
	if aokS && bokS {
		return strings.Compare(as, bs)
	}
	return 0
}

// rowMatchGroups 逐行求值 OR 连接的 AND 条件组。
func rowMatchGroups(row map[string]interface{}, orGroups []andGroup) bool {
	for _, g := range orGroups {
		ok := true
		for _, c := range g {
			if !evalCond(row[c.field], c) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// evalCond 单条条件求值。数值/字符串可比较时按值比较；类型不可比时
// 仅 = / <> 有意义（严格相等），范围比较返回 false。
func evalCond(v interface{}, c *rawCond) bool {
	switch c.op {
	case "ISNULL":
		return v == nil
	case "ISNOTNULL":
		return v != nil
	case "LIKE":
		s, ok1 := v.(string)
		pat, ok2 := c.val.(string)
		return ok1 && ok2 && likeMatch(s, pat)
	}
	av, aok := toNum(v)
	bv, bok := toNum(c.val)
	if aok && bok {
		switch c.op {
		case "=":
			return av == bv
		case "<>", "!=":
			return av != bv
		case ">":
			return av > bv
		case "<":
			return av < bv
		case ">=":
			return av >= bv
		case "<=":
			return av <= bv
		case "BETWEEN":
			b2, b2ok := toNum(c.val2)
			return b2ok && av >= bv && av <= b2
		}
		return false
	}
	vs, vok := v.(string)
	bs, bokS := c.val.(string)
	if vok && bokS {
		switch c.op {
		case "=":
			return vs == bs
		case "<>", "!=":
			return vs != bs
		case ">":
			return vs > bs
		case "<":
			return vs < bs
		case ">=":
			return vs >= bs
		case "<=":
			return vs <= bs
		case "BETWEEN":
			b2, b2ok := c.val2.(string)
			return b2ok && vs >= bs && vs <= b2
		}
		return false
	}
	// 类型不可比：只支持严格相等判断（SQL 中 = NULL 恒为假）
	if c.op == "=" {
		return v == nil && c.val == nil
	}
	if c.op == "<>" || c.op == "!=" {
		return !(v == nil && c.val == nil)
	}
	return false
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
func (db *DB) queryMysqlTable(name string, conds map[string]interface{}, selectCols []string, colSrc map[string]string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
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
			row = append(row, rec[strings.ToLower(srcColumn(colSrc, c))])
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
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTableIn(tableName, p.curSchema(db))
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
	// 多行 INSERT 整体原子：包一个隐式事务，任一行失败即整体回滚
	//（MySQL 语句级原子性）；会话已在事务中（p.txnID != 0）时沿用外层事务。
	autoTxn := p.txnID == 0
	if autoTxn {
		p.txnID = db.BeginTxn()
	}
	rollback := func(e error) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
		if autoTxn {
			db.RollbackTxn(p.txnID)
			p.txnID = 0
		}
		return nil, nil, 0, "", e
	}
	affected = 0
	for {
		if q := p.next(); q.kind != "sym" || q.val != "(" {
			return rollback(fmt.Errorf("expected ("))
		}
		var values []interface{}
		if colsExplicit {
			values = make([]interface{}, 0, len(fields))
		} else {
			values = interfaceSlicePool.Get().([]interface{})[:0]
			if cap(values) < len(t.Meta().Fields) {
				values = make([]interface{}, 0, len(t.Meta().Fields))
			}
		}
		for {
			v, e := p.parseValue()
			if e != nil {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				return rollback(e)
			}
			values = append(values, v)
			if p.matchSym(")") {
				break
			}
			if q := p.next(); q.kind != "sym" || q.val != "," {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				return rollback(fmt.Errorf("expected , or )"))
			}
		}
		// 提交单行
		if e := db.insertRow(p, t, fields, colsExplicit, values); e != nil {
			if !colsExplicit {
				interfaceSlicePool.Put(values[:0])
			}
			return rollback(e)
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
	if e := p.expectEnd(); e != nil {
		return rollback(e)
	}
	if autoTxn {
		if e := db.CommitTxn(p.txnID); e != nil {
			p.txnID = 0
			return nil, nil, 0, "", e
		}
		p.txnID = 0
	}
	rawMsg = fmt.Sprintf("%d row inserted", affected)
	if affected > 1 {
		rawMsg = fmt.Sprintf("%d rows inserted", affected)
	}
	return
}

// insertRow 提交 INSERT 的单行：优先位置参数快速路径，否则按列名映射。
// 事务模式下记录 read-version 与 Changes——CommitTxn 依赖 Changes 写出
// COMMIT 记录并在冲突时回滚，缺失会让重放丢行。
func (db *DB) insertRow(p *sqlParser, t *Table, fields []string, colsExplicit bool, values []interface{}) error {
	// 快速路径：位置参数（字段序即表字段序）直接编码，跳过 map 构建。
	// 显式列清单仅在列名与表字段序完全一致时才可走快速路径，否则 values 与
	// meta.Fields 错位会静默串列写坏数据（含 WAL，重放后同样错）。
	orderedPath := !colsExplicit
	if colsExplicit && len(fields) == len(t.Meta().Fields) {
		orderedPath = true
		for i, f := range t.Meta().Fields {
			if fields[i] != f.Name {
				orderedPath = false
				break
			}
		}
	}
	pkIdx := -1
	if orderedPath {
		if len(values) != len(t.Meta().Fields) {
			return fmt.Errorf("column count mismatch")
		}
		for i, f := range t.Meta().Fields {
			if f.Name == t.Meta().PK {
				pkIdx = i
				break
			}
		}
	} else {
		if len(fields) != len(values) {
			return fmt.Errorf("column count mismatch")
		}
		for i, f := range fields {
			if f == t.Meta().PK {
				pkIdx = i
				break
			}
		}
	}
	var pk int64
	if pkIdx >= 0 && pkIdx < len(values) {
		v, ok := values[pkIdx].(int64)
		if !ok {
			return fmt.Errorf("primary key must be integer")
		}
		pk = v
	} else if colsExplicit {
		rowMap := make(map[string]interface{}, len(fields))
		for i, f := range fields {
			rowMap[f] = values[i]
		}
		pkVal, ok := rowMap[t.Meta().PK]
		if !ok {
			return fmt.Errorf("missing primary key field %s", t.Meta().PK)
		}
		pkv, ok := pkVal.(int64)
		if !ok {
			return fmt.Errorf("primary key must be integer")
		}
		pk = pkv
	} else {
		return fmt.Errorf("missing primary key field %s", t.Meta().PK)
	}
	if p.txnID != 0 {
		var oldData []byte
		if d, ok := t.pkTree.Get(pk); ok {
			oldData = d
			db.recordReadVersion(p.txnID, t.Meta().Name, pk, decodeVersion(d))
		}
		db.txnMu.Lock()
		if txn, ok := db.txns[p.txnID]; ok {
			tname := t.Meta().Name
			if txn.Changes[tname] == nil {
				txn.Changes[tname] = make(map[int64][]byte)
			}
			txn.Changes[tname][pk] = oldData
			txn.Tables[tname] = true
		}
		db.txnMu.Unlock()
	}
	if orderedPath && pkIdx >= 0 && pkIdx < len(values) {
		return t.InsertOrdered(pk, values, 0, p.txnID, false)
	}
	rowMap := make(map[string]interface{}, len(fields))
	for i, f := range fields {
		rowMap[f] = values[i]
	}
	return t.Insert(pk, rowMap, 0, p.txnID, false)
}

func (db *DB) parseUpdate(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // UPDATE
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTableIn(tableName, p.curSchema(db))
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
	if pkField != t.Meta().PK {
		err = fmt.Errorf("UPDATE WHERE must use primary key %s", t.Meta().PK)
		return
	}
	pk, ok := pkVal.(int64)
	if !ok {
		err = fmt.Errorf("primary key must be integer")
		return
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	old, e := t.Select(map[string]interface{}{t.Meta().PK: pk}, nil, nil)
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
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTableIn(tableName, p.curSchema(db))
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
	if pkField != t.Meta().PK {
		err = fmt.Errorf("DELETE WHERE must use primary key %s", t.Meta().PK)
		return
	}
	pk, ok := pkVal.(int64)
	if !ok {
		err = fmt.Errorf("primary key must be integer")
		return
	}
	if e := p.expectEnd(); e != nil {
		err = e
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
	name, e := p.readQualifiedTable()
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
	name, e := p.readQualifiedTable()
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
//
// 仅支持向表末尾追加列与重命名；不改变既有字段顺序（避免破坏既有 WAL 行编码）。
func (db *DB) parseAlterTable(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // ALTER
	if !p.matchKeyword("TABLE") {
		err = fmt.Errorf("expected TABLE")
		return
	}
	name, e := p.readQualifiedTable()
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
		// 追加新字段（保持既有字段序），重写 catalog。
		// 必须走引擎的写时复制路径：published meta 不可原地修改，
		// 否则并发 SELECT 无锁读 Fields 切片会撕裂（回归测试 TestAlterAddColumnCOW）。
		if we := db.AlterTableAddColumn(name, fname, ftype, flen); we != nil {
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
		// 写时复制：发布改名后的新 meta 快照，避免原地修改读者可能持有的共享 TableMeta。
		db.mu.Lock()
		oldName := t.Meta().Name
		oldID := walRegRegister(oldName)
		t.setMeta(cloneMeta(t.Meta(), newname))
		db.tables.Delete(oldName)
		db.tables.Store(newname, t)
		delete(db.catalog, oldName)
		db.catalog[newname] = t.Meta()
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
// 迭代式两指针算法（记录最近 % 回溯点）：最坏 O(len(s)×len(p))，
// 不会像朴素的 % 逐位置递归那样指数爆炸，也不会因 MB 级模式打爆 goroutine 栈。
func likeMatch(s, pattern string) bool {
	ls, lp := strings.ToLower(s), strings.ToLower(pattern)
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(ls) {
		if pi < len(lp) && (lp[pi] == '_' || lp[pi] == ls[si]) {
			si++
			pi++
		} else if pi < len(lp) && lp[pi] == '%' {
			starP = pi
			starS = si
			pi++
		} else if starP >= 0 {
			pi = starP + 1
			starS++
			si = starS
		} else {
			return false
		}
	}
	for pi < len(lp) && lp[pi] == '%' {
		pi++
	}
	return pi == len(lp)
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
