package main

import (
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ==================== SQL 解析与执行 ====================

// sqlTokenKind token 类别（byte 常量：取代 string 比较并把结构体从 72B
// 压到 40B——token 追加/消费是分词与解析热路径上最大的 memcpy）。
type sqlTokenKind uint8

const (
	kindKw    sqlTokenKind = iota // 关键字
	kindIdent                     // 标识符
	kindStr                       // 字符串
	kindNum                       // 数字
	kindSym                       // 符号
	kindEof                       // 结束
)

// sqlToken 词法单元。
// upper 缓存标识符/关键字的大写形式，避免后续解析重复 strings.ToUpper。
type sqlToken struct {
	kind  sqlTokenKind
	val   string
	upper string // 仅对 ident/kw 非空
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
	// 按 SQL 长度预估 token 数（平均 ≥3 字节/token）一次到位，避免中途
	// 扩容拷贝（sqlToken 72B，长查询的反复扩容不小）；超大语句不预分配。
	if est := len(sql)/3 + 4; cap(toks) < est && est <= 4096 {
		toks = make([]sqlToken, 0, est)
	}
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
			toks = append(toks, sqlToken{kind: kindStr, val: val})
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
			toks = append(toks, sqlToken{kind: kindIdent, val: val})
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
			toks = append(toks, sqlToken{kind: kindNum, val: sql[i:j]})
			i = j
		case isIdentChar(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			word := sql[i:j]
			// 关键字：内部化 canonical 大写（零分配）；普通标识符不再预计算
			// ToUpper（旧实现每个含小写字母的 token 一次堆分配，是分词热路径
			// 的主要分配源）。ident 的大小写不敏感比较由消费方 asciiEq 完成。
			kind := kindIdent
			var upper string
			if c := keywordCanonical(word); c != "" {
				kind, upper = kindKw, c
			}
			toks = append(toks, sqlToken{kind: kind, val: word, upper: upper})
			i = j
		default:
			toks = append(toks, sqlToken{kind: kindSym, val: symChar(c)})
			i++
		}
	}
	toks = append(toks, sqlToken{kind: kindEof, val: ""})
	return toks, nil
}

// scanQuoted 扫描单/双引号字符串，处理反斜杠转义与
// 引号 doubling（连续两个相同引号表示字面引号），返回解码后的值与结束后的下一个下标。
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

// asciiEq ASCII 专用的大小写不敏感比较（关键字词表全为大写 ASCII，语义与
// strings.EqualFold 一致：非 ASCII 字节不可能折叠进纯 ASCII 词表）。
// 折叠仅限 'a'~'z'——无上界的 >= 'a' 会把 '{'|'~' 等标点折叠成 '['|'^'，
// 与词表字节发生假相等。取代 EqualFold 的 Unicode 逐字符折叠——分词/解析
// 热路径上每 token 都要走。
func asciiEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca != cb {
			if ca >= 'a' && ca <= 'z' {
				ca -= 'a' - 'A'
			}
			if cb >= 'a' && cb <= 'z' {
				cb -= 'a' - 'A'
			}
			if ca != cb {
				return false
			}
		}
	}
	return true
}

// kwByFirst 按首字母（折叠为小写）分桶的关键字表：keywordCanonical 对每个
// 标识符只扫描同首字母候选（通常 1~3 个），再做 ASCII 折叠比较，零分配。
var kwByFirst [26][][]string // [首字母][长度][候选词]

func init() {
	for _, k := range sqlKeywords {
		c := k[0] | 0x20 // 已折叠小写（关键字均为大写 ASCII）
		idx := c - 'a'
		if int(idx) >= len(kwByFirst) {
			continue
		}
		buckets := kwByFirst[idx]
		if len(k) >= len(buckets) {
			grown := make([][]string, len(k)+1)
			copy(grown, buckets)
			buckets = grown
			kwByFirst[idx] = buckets
		}
		buckets[len(k)] = append(buckets[len(k)], k)
	}
}

// keywordCanonical 若 word 是关键字（大小写不敏感），返回内部化的大写形式
// （零分配）；否则返回空串。取代旧的 isKeyword + strings.ToUpper 组合——
// 后者对每个含小写字母的 token 都产生一次堆分配。
func keywordCanonical(word string) string {
	if len(word) == 0 {
		return ""
	}
	c := word[0] | 0x20
	if c < 'a' {
		return "" // 非字母首字符不可能命中关键字
	}
	idx := int(c - 'a')
	if idx >= len(kwByFirst) {
		return ""
	}
	buckets := kwByFirst[idx]
	if len(word) >= len(buckets) {
		return ""
	}
	for _, k := range buckets[len(word)] {
		if asciiEq(k, word) {
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
	user     string        // 执行账号（空 = 内部可信调用，不做表级授权）
	// out：语句级输出容器（自增主键等）。协议层用它回填 LastInsertId。
	out *sqlOut
	// lastInsertID：本语句由引擎分配的自增主键（0 = 未分配）
	lastInsertID int64
}

// sqlOut 承载语句级输出，供协议层填充响应（如 MySQL OK 包里的 last insert id）。
type sqlOut struct {
	LastInsertID int64
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
// 非关键字字面量）走 ASCII 折叠比较——词表全为大写 ASCII，语义与
// EqualFold 一致且更快。
func (p *sqlParser) matchKeyword(kw string) bool {
	t := p.peek()
	if t.kind == kindKw {
		if t.upper == kw {
			p.next()
			return true
		}
		return false
	}
	if t.kind == kindIdent && asciiEq(t.val, kw) {
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
	return asciiEq(t.val, word)
}

// stmtWords 保留字表之外的语句级语法词：它们不在 sqlKeywords 中（避免侵占
// 标识符命名空间），但作为语句首词/子句词参与分派，需大小写不敏感归一。
var stmtWords = []string{"ALTER", "SETS", "DATABASE", "USE"}

// canonical 返回 token 的规整大写形式：关键字用内部化 upper（零分配），
// 其余按 stmtWords 大小写不敏感解析（ASCII 折叠）。
func (t sqlToken) canonical() string {
	if t.upper != "" {
		return t.upper
	}
	if t.kind != kindIdent {
		return ""
	}
	for _, w := range stmtWords {
		if asciiEq(t.val, w) {
			return w
		}
	}
	return ""
}

func (p *sqlParser) matchSym(s string) bool {
	t := p.peek()
	if t.kind == kindSym && t.val == s {
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

// runSQLAs 以指定账号执行 SQL：内部统一做表级权限校验（二进制 / MySQL 协议入口）。
func (db *DB) runSQLAs(user, sql string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLSchemaAs(user, sql, nil, 0, "")
}

// runSQLAsOut 与 runSQLAs 相同，但把语句级输出（自增主键）写进 out。
func (db *DB) runSQLAsOut(user, sql string, out *sqlOut) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLSchemaAsOut(user, sql, nil, 0, "", out)
}

// runSQLSchemaAs 以指定账号执行 SQL（带连接级默认库）。user=="" 表示内部可信调用。
func (db *DB) runSQLSchemaAs(user, sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLSchemaAsOut(user, sql, params, txnID, schema, nil)
}

// runSQLSchemaAsOut 与 runSQLSchemaAs 相同，但把语句级输出（自增主键）写进 out。
func (db *DB) runSQLSchemaAsOut(user, sql string, params []interface{}, txnID uint64, schema string, out *sqlOut) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	toks, terr := tokenizeSQL(sql)
	if terr != nil {
		err = terr
		return
	}
	defer putSQLTokens(toks)
	return db.runSQLTokensAsOut(user, toks, sql, params, txnID, schema, out)
}

// runSQLTokens 使用调用方提供的已分词 tokens 执行（prepared 复用，避免每次 EXECUTE 重新分词）。
func (db *DB) runSQLTokens(toks []sqlToken, sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLTokensAs("", toks, sql, params, txnID, schema)
}

// runSQLTokensAs 是 runSQLTokens 的带账号版本：user 非空时先做表级授权检查。
func (db *DB) runSQLTokensAs(user string, toks []sqlToken, sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	return db.runSQLTokensAsOut(user, toks, sql, params, txnID, schema, nil)
}

// runSQLTokensAsOut 与 runSQLTokensAs 相同，但把语句级输出（自增主键）写进 out。
// 协议层用它回填 MySQL OK 包的 last insert id（GORM 等 ORM 依赖它取自增主键）。
func (db *DB) runSQLTokensAsOut(user string, toks []sqlToken, sql string, params []interface{}, txnID uint64, schema string, out *sqlOut) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	if user != "" {
		if aerr := db.authorizeSQLTokensFast(user, toks); aerr != nil {
			err = aerr
			return
		}
	}
	// UNION [ALL]：按 token 拆成左右两个 SELECT 分别执行后合并
	if idx, all, hasUnion := findTopLevelUnion(toks); hasUnion {
		return db.runUnionAs(user, toks, idx, all, sql, params, txnID, schema)
	}
	p := &sqlParser{toks: toks, params: params, txnID: txnID, schema: schema, user: user, out: out}
	if out != nil {
		// 语句结束后把自增主键回填给协议层（MySQL OK 包的 last insert id）
		defer func() { out.LastInsertID = p.lastInsertID }()
	}
	// 去除开头的空/分号
	for p.peek().kind == kindSym && p.peek().val == ";" {
		p.next()
	}
	first := p.peek()
	if first.kind == kindEof {
		err = fmt.Errorf("empty query")
		return
	}
	// 语句首词归一：JOIN/BEGIN/REPLACE/TRUNCATE 等不在关键字表里（避免占用
	// 标识符命名空间），必须按 token 原文比较，否则这些语句会落到 default 报错。
	switch strings.ToUpper(first.val) {
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
				if d := p.next(); d.kind == kindIdent || d.kind == kindStr {
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
		// SHOW MASTER STATUS / SHOW BINARY LOGS / SHOW SLAVE STATUS
		if p.matchKeyword("MASTER") {
			p.matchKeyword("STATUS")
			columns = []string{"File", "Position", "Binlog_Do_DB", "Binlog_Ignore_DB", "Executed_Gtid_Set"}
			rows = [][]interface{}{{"tsumugi-bin.000001", int64(4), "", "", ""}}
			return
		}
		if p.matchKeyword("BINARY") {
			p.matchKeyword("LOGS")
			columns = []string{"Log_name", "File_size"}
			rows = [][]interface{}{{"tsumugi-bin.000001", int64(0)}}
			return
		}
		if p.matchKeyword("SLAVE") {
			p.matchKeyword("STATUS")
			p.matchKeyword("HOSTS")
			columns = []string{"Slave_IO_State", "Master_Host", "Master_Port"}
			rows = nil
			return
		}
		if p.matchKeyword("PRIVILEGES") {
			columns = []string{"Privilege", "Context", "Comment"}
			rows = [][]interface{}{{"Select", "Tables", ""}, {"Insert", "Tables", ""}, {"Update", "Tables", ""}, {"Delete", "Tables", ""}, {"Create", "Databases,Tables,Indexes", ""}, {"Drop", "Databases,Tables", ""}, {"Alter", "Tables", ""}, {"Index", "Tables", ""}, {"Grant option", "Databases,Tables", ""}}
			return
		}
		if p.matchKeyword("PROCEDURE") {
			p.matchKeyword("STATUS")
			columns = []string{"Db", "Name", "Type", "Definer", "Modified", "Created"}
			rows = nil
			return
		}
		if p.matchKeyword("FUNCTION") {
			p.matchKeyword("STATUS")
			columns = []string{"Db", "Name", "Type", "Definer", "Modified", "Created"}
			rows = nil
			return
		}
		if p.matchKeyword("OPEN") {
			p.matchKeyword("TABLES")
			columns = []string{"Database", "Table", "In_use", "Name_locked"}
			rows = nil
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
				if t := p.next(); t.kind == kindStr {
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
		if nameTok.kind != kindIdent && nameTok.kind != kindStr {
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
			if nameTok.kind != kindIdent && nameTok.kind != kindStr {
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
			if nameTok.kind != kindIdent && nameTok.kind != kindStr {
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
		if nameTok.kind != kindIdent && nameTok.kind != kindStr {
			err = fmt.Errorf("expected table name")
			return
		}
		// 单表直查：不走 adminTables（会为所有表重建元数据 map）——
		// phpMyAdmin 每次浏览都发 DESCRIBE，这里曾是 O(表数×字段数) 的分配。
		// 物理表名带 db. 前缀：裸名先精确匹配，再按当前连接库限定。
		tbl := db.getTable(nameTok.val)
		if tbl == nil {
			qual := db.qualifyTableIn(nameTok.val, p.curSchema(db))
			if qual != nameTok.val {
				tbl = db.getTable(qual)
			}
		}
		if tbl == nil {
			err = fmt.Errorf("table not found: %s", nameTok.val)
			return
		}
		dmeta := tbl.Meta()
		columns = []string{"field", "type", "len", "key"}
		for _, f := range dmeta.Fields {
			key := ""
			if dmeta.PK == f.Name {
				key = "PRI"
			}
			rows = append(rows, []interface{}{f.Name, fieldTypeName(f.Type), f.Len, key})
		}
		// map 遍历顺序随机，按索引名排序保证 DESCRIBE 输出确定性
		//（phpMyAdmin 每次浏览都发 DESCRIBE，行序不稳定会在页面间闪变）。
		idxNames := make([]string, 0, len(dmeta.Indexes))
		for idxName := range dmeta.Indexes {
			idxNames = append(idxNames, idxName)
		}
		sort.Strings(idxNames)
		for _, idxName := range idxNames {
			rows = append(rows, []interface{}{dmeta.Indexes[idxName], "INDEX(" + idxName + ")", "", "MUL"})
		}
		return
	case "SELECT":
		return db.parseSelect(p)
	case "INSERT":
		return db.parseInsert(p)
	case "REPLACE":
		// REPLACE INTO：与 INSERT 同一解析路径（replaceMode）
		return db.parseInsert(p)
	case "UPDATE":
		return db.parseUpdate(p)
	case "DELETE":
		return db.parseDelete(p)
	case "ALTER":
		return db.parseAlterTable(p)
	case "TRUNCATE":
		return db.parseTruncate(p)
	case "RENAME":
		return db.parseRenameTable(p)
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
		// 语句级原子性由写语句自身的隐式事务保证；这里保持客户端语义兼容
		return db.parseTxnControl(p)
	case "FLUSH", "ANALYZE", "OPTIMIZE", "CHECK", "REPAIR", "LOCK", "UNLOCK",
		"KILL", "DO", "HELP", "RESET", "SET", "SETS":
		// SET 的完整语义（autocommit 等）由协议层处理；这里统一返回 OK
		return db.parseNoOpStmt(p)
	case "CALL":
		err = fmt.Errorf("stored procedures are not supported")
		return
	}
	err = fmt.Errorf("unsupported statement: %s", first.val)
	return
}

// countSQLTokens 统计 token 列表中的占位符数量（供 prepared 使用）。
func countSQLTokens(toks []sqlToken) int {
	n := 0
	for _, t := range toks {
		if t.kind == kindSym && t.val == "?" {
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
	ok := t.kind == kindIdent || t.kind == kindStr || (allowKeyword && t.kind == kindKw)
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
	for p.peek().kind == kindSym && p.peek().val == "." {
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
	if d := p.next(); d.kind == kindIdent || d.kind == kindStr {
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
		if op.kind != kindSym || op.val != "=" {
			return nil, fmt.Errorf("unsupported condition in COUNT")
		}
		v, e := p.parseValue()
		if e != nil {
			return nil, e
		}
		conds[f] = v
		if !p.matchKeyword("AND") {
			// OR / 其它连接词：明确报错，别让 COUNT 静默只按第一个条件计数
			if t := p.peek(); t.kind == kindKw && t.upper == "OR" {
				return nil, fmt.Errorf("OR conditions are not supported in COUNT(*)")
			}
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
	if t.kind == kindSym && t.val == "?" {
		v := p.nextParam()
		if v == nil {
			return nil, fmt.Errorf("no value bound for parameter %d", p.paramIdx+1)
		}
		return v, nil
	}
	switch t.kind {
	case kindNum:
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
	case kindStr:
		return t.val, nil
	case kindKw:
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
// 元素为值类型：条件随查询结束即弃，值切片省去每条件一次堆分配。
type andGroup []rawCond

func (db *DB) parseSelect(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // SELECT
	// 无 FROM 的标量查询（SELECT @@version / SELECT VERSION() 等）保持原快路径
	if c, r, e := db.queryScalarSelect(p); r != nil {
		columns, rows, err = c, r, e
		return
	}
	// 表达式投影 / JOIN / GROUP BY / DISTINCT / UNION 等走通用路径，
	// 简单查询继续走下面的字节级快路径（性能不受影响）。
	if p.selectNeedsGeneral() {
		return db.parseSelectGeneral(p)
	}
	// 无 FROM 的标量查询（SELECT @@version, SELECT VERSION(), SELECT NOW() 等）
	if c, r, e := db.queryScalarSelect(p); r != nil {
		columns, rows, err = c, r, e
		return
	}
	// COUNT 聚合：SELECT COUNT(*) FROM tbl [WHERE ..] / SELECT COUNT(1) 等
	isCount := p.peek().is("COUNT") && p.toks[p.i+1].kind == kindSym && p.toks[p.i+1].val == "("
	if isCount {
		p.next() // COUNT
		p.next() // (
		for {
			t := p.next()
			if t.kind == kindSym && t.val == ")" {
				break
			}
			if t.kind == kindEof {
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
	var colSrc map[string]string // 输出列名 -> 源列名（AS 别名映射；无别名时不分配）
	if pt := p.peek(); pt.kind == kindSym && pt.val == "*" {
		p.next()
		selectCols = nil // 表示所有列
	} else {
		readCol := func() (string, error) {
			ct := p.peek()
			if ct.kind != kindIdent && ct.kind != kindKw && ct.kind != kindStr {
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
			if p.peek().kind == kindSym && p.peek().val == "." {
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
				if colSrc == nil {
					colSrc = make(map[string]string)
				}
			}
			selectCols = append(selectCols, colName)
			if colSrc != nil {
				colSrc[strings.ToLower(colName)] = src
			}
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
	if !hasPrefixFold(tableName, "mysql.") {
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
			addCond := func(c rawCond) {
				*cur = append(*cur, c)
			}
			if p.matchKeyword("IS") {
				neg := p.matchKeyword("NOT")
				if !p.matchKeyword("NULL") {
					err = fmt.Errorf("expected NULL after IS")
					return
				}
				if neg {
					addCond(rawCond{field: field, op: "ISNOTNULL"})
				} else {
					addCond(rawCond{field: field, op: "ISNULL"})
				}
			} else {
				op := p.next()
				// BETWEEN / LIKE 是关键字(kw)，其余比较符是符号(sym)
				isKwOp := (op.kind == kindKw || op.kind == kindIdent) && (op.upper == "BETWEEN" || op.upper == "LIKE")
				if op.kind != kindSym && !isKwOp {
					err = fmt.Errorf("unsupported comparison operator")
					return
				}
				// tokenizer 会把 >= <= <> != 拆成两个单字符符号，这里合并
				if op.val == "<" && p.peek().kind == kindSym && p.peek().val == ">" {
					p.next()
					op.val = "<>"
				}
				if (op.val == ">" || op.val == "<" || op.val == "!") && p.peek().kind == kindSym && p.peek().val == "=" {
					p.next() // 消费 "="，组成 ">=" "<=" "!="
					if op.val == "!" {
						op.val = "<>"
					} else {
						op.val = op.val + "="
					}
				}
				// 比较符归一：符号本身即 canonical；关键字（BETWEEN/LIKE）取
				// 内部化大写 upper，免去每条件两次 ToUpper 堆分配。
				cmp := op.val
				if op.upper != "" {
					cmp = op.upper
				}
				switch cmp {
				case "=", ">", "<", ">=", "<=", "<>", "!=":
					val, e := p.parseValue()
					if e != nil {
						err = e
						return
					}
					addCond(rawCond{field: field, op: cmp, val: val})
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
					addCond(rawCond{field: field, op: "BETWEEN", val: lo, val2: hi})
				case "LIKE":
					pat, e := p.parseValue()
					if e != nil {
						err = e
						return
					}
					addCond(rawCond{field: field, op: "LIKE", val: pat})
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
		if num.kind != kindNum {
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
			if num2.kind != kindNum {
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
			if num2.kind != kindNum {
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
	if hasPrefixFold(tableName, "mysql.") {
		mt := strings.ToLower(strings.TrimPrefix(tableName, "mysql."))
		conds := condsFromGroups(orGroups)
		if conds == nil && len(orGroups) > 1 {
			err = fmt.Errorf("OR in WHERE is not supported for system tables")
			return
		}
		return db.queryMysqlTable(mt, conds, selectCols, colSrc)
	}
	// information_schema.* 虚拟系统表
	if hasPrefixFold(tableName, "information_schema.") {
		it := strings.ToLower(strings.TrimPrefix(tableName, "information_schema."))
		conds := condsFromGroups(orGroups)
		if conds == nil && len(orGroups) > 1 {
			err = fmt.Errorf("OR in WHERE is not supported for system tables")
			return
		}
		columns, rows, err = db.queryInfoSchema(it, conds, selectCols, colSrc)
		return
	}
	// 系统表（__tables / __indexes / __stats）：此前只有二进制协议能查，
	// README 却写着"可直接 SELECT"，这里补齐 SQL 路径。
	if base := tableName[strings.LastIndexByte(tableName, '.')+1:]; strings.HasPrefix(base, "__") {
		columns, rows, err = db.systemTableSelect(base, selectCols, colSrc)
		return
	}
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	meta := t.Meta()
	// 单一 AND 组且无 OR 时走索引/范围下推快速路径；含 OR 时退化为全表谓词过滤
	hasOR := len(orGroups) > 1
	// 主键点查快速路径：单一 `pk = 整数常量` 条件且无 ORDER BY / OFFSET 时，
	// 跳过条件 map 构建与 SelectR 的通用分发，直接点查后按列投影。
	pkPoint := false
	var pkPointVal int64
	if len(orGroups) == 1 && len(orGroups[0]) == 1 && len(orderBys) == 0 && offset == 0 {
		c := orGroups[0][0]
		if c.op == "=" && strings.EqualFold(c.field, meta.PK) {
			if v, ok := c.val.(int64); ok {
				pkPointVal, pkPoint = v, true
			}
		}
	}
	// conds 懒分配：主键点查与无 WHERE 路径不需要条件 map
	var conds map[string]interface{}
	var minKey, maxKey *int64
	var filters []RangeCond
	// contradiction 表示本 AND 组恒不命中（任一端值为 NULL，SQL 里与 NULL
	// 比较结果是 UNKNOWN）。用反向主键区间让扫描直接空转，
	// 而不是把 NULL 当 0 下推（旧实现 `id > NULL` / `id > '500'` 会返回全表）。
	contradiction := false
	if len(orGroups) == 1 && !pkPoint {
		for _, c := range orGroups[0] {
			if c.op != "ISNULL" && c.op != "ISNOTNULL" &&
				(c.val == nil || (c.op == "BETWEEN" && c.val2 == nil)) {
				contradiction = true
				continue
			}
			if c.op == "=" {
				if conds == nil {
					conds = make(map[string]interface{})
				}
				conds[c.field] = c.val
				continue
			}
			if strings.EqualFold(c.field, meta.PK) {
				if c.op == "LIKE" || c.op == "ISNULL" || c.op == "ISNOTNULL" {
					hasOR = true
					continue
				}
				if c.op == "<>" {
					// 单区间无法表达"不等于"：退化为全表扫描 + ne 过滤
					hasOR = true
					if c.val != nil {
						filters = append(filters, RangeCond{Field: c.field, Op: "ne", Val: c.val})
					}
					continue
				}
				iv, iok := toInt64OK(c.val)
				var iv2 int64
				if c.op == "BETWEEN" {
					iv2, iok = toInt64OK(c.val2)
				}
				if !iok {
					// 非数值界值（如 'abc'）：不下推，交给通用过滤，避免静默错结果
					hasOR = true
					filters = append(filters, RangeCond{Field: c.field, Op: rangeOp(c.op), Val: c.val, Val2: c.val2})
					continue
				}
				switch c.op {
				case ">":
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
					if minKey == nil || *minKey < iv {
						minKey = &iv
					}
				case "<":
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
					if maxKey == nil || *maxKey > iv {
						maxKey = &iv
					}
				case "BETWEEN":
					if minKey == nil || *minKey < iv {
						minKey = &iv
					}
					if maxKey == nil || *maxKey > iv2 {
						maxKey = &iv2
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
	// 恒不命中：反向主键区间让所有扫描路径直接空转（比扫全表再过滤更快）
	if contradiction {
		zero, one := int64(0), int64(1)
		minKey, maxKey = &one, &zero
	}
	// 列投影准备：SELECT * 返回全部列（colIdx 为 nil 表示恒等投影，行值直接
	// copy）；否则只返回指定的列（AS 别名映射到源列），colIdx 记录各输出列
	// 在 meta.Fields 中的下标。
	cols := meta.Fields
	var colIdx []int
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
			for i, f := range meta.Fields {
				if strings.EqualFold(src, f.Name) {
					cols = append(cols, f)
					colIdx = append(colIdx, i)
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
		outNames = meta.fieldNames() // SELECT *：列头取 meta 惰性缓存
	}
	columns = outNames
	// fieldToOut 把源字段下标映射到输出列下标（-1 跳过）。扫描时只解码
	// 选中列，避免 SELECT id FROM ... 仍解码整行 varchar 并二次投影。
	var fieldToOut []int
	if colIdx != nil {
		fieldToOut = make([]int, len(meta.Fields))
		for i := range fieldToOut {
			fieldToOut[i] = -1
		}
		for out, ci := range colIdx {
			fieldToOut[ci] = out
		}
	}
	// projectRow 将 flat 解码行投影到 arena 行（恒等投影走 copy，零下标循环）。
	projectRow := func(flat, row []interface{}) {
		if colIdx == nil {
			copy(row, flat)
			return
		}
		for i, ci := range colIdx {
			row[i] = flat[ci]
		}
	}
	// ORDER BY / OFFSET 需要完整结果集后再裁剪，扫描阶段不能提前 limit
	scanLimit := limit
	if hasOR || len(orderBys) > 0 || offset > 0 {
		scanLimit = -1
	}
	// 内存保护：SQL 路径会把结果集整体物化成 [][]interface{}。没有 LIMIT 时
	// 给扫描加一个硬上限，超过就报错而不是把进程撑爆（小内存设备上的 OOM 主因）。
	// 多扫 1 行用于判断"是否超限"。
	scanCapped := limit < 0 && scanLimit < 0
	if scanCapped {
		scanLimit = maxScanRows + 1
	}
	// 主键点查：树内直接命中 + TTL 检查 + flat 单行投影，避开通用扫描路径
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
		row := make([]interface{}, len(cols))
		decodeRowProjected(meta, rowData, row, fieldToOut)
		rows = [][]interface{}{row}
		return
	}
	// 扫描 + 过滤 + 投影
	if hasOR {
		// 条件字面量先按列类型折算（DATE/DATETIME → 纳秒，DECIMAL → 定点整数），
		// 这条路径用 decodeRowFlat 的原始值做 Go 层比较，不经过 scanConds 的
		// 类型折算；不折算会让 `WHERE amount = 12.34 OR id = 1` 拿 1234 与
		// 12.34 直接比较而漏行。
		normalizeGroupCondValues(meta, orGroups)
		// 全表扫描 + 逐行谓词求值（OR 或 LIKE / IS NULL 等不可下推条件）：
		// 条件字段预解析为下标，行 flat 解码后求值、命中行内联投影，
		// 全程不为扫描行物化 map（此前每行一个 map 是该路径最大分配源）。
		gIdx := make([][]int, len(orGroups))
		for gi, g := range orGroups {
			idxs := make([]int, len(g))
			for ci := range g {
				idxs[ci] = meta.fieldIndexOf(g[ci].field)
			}
			gIdx[gi] = idxs
		}
		var expired []int64
		now := time.Now().UnixNano()
		var flat []interface{}
		// stride 用 len(cols)：SELECT * 时 colIdx 为 nil（恒等投影），len(colIdx)=0
		// 会把行缓冲钳成 1 列、命中行被截断；len(cols) 在两种投影下都是正确行宽。
		arena := newRowArena(len(cols))
		rows = make([][]interface{}, 0, 64)
		t.pkTree.scanRangeStop(minKey, maxKey, func(key int64, value []byte) bool {
			if e := decodeExpireAt(value); e > 0 && now > e {
				expired = append(expired, key)
				return true
			}
			flat = decodeRowFlat(meta, value, flat)
			if !rowMatchGroupsFlat(flat, orGroups, gIdx) {
				return true
			}
			row := arena.row()
			projectRow(flat, row)
			rows = append(rows, row)
			return true
		})
		for _, k := range expired {
			t.pkTree.Delete(k)
		}
	} else if len(conds) == 0 && len(filters) == 0 {
		// 无 WHERE 条件的快速路径：只解码选中列，跳过未选中的 varchar 等字段，
		// 也不再生成完整 flat 行后二次投影。
		rows = t.selectRowsProjected(minKey, maxKey, scanLimit, fieldToOut, len(cols))
	} else if cov, okCov := t.coveringRows(meta, conds, filters, colIdx, len(cols), scanLimit); okCov {
		// 覆盖索引：投影列全在二级索引条目里（索引列 + 主键），
		// 直接从索引构造结果，省掉回表（主键树查找 + 行解码）。
		rows = cov
	} else {
		// 等值/范围条件路径：与 SelectR 相同的字节级求值内核
		//（主键点查/二级索引/全表扫描三级分发），命中行 flat 解码后
		// 按 colIdx 直接投影，全程不物化行 map。
		rawRows := t.selectRBytes(conds, filters, minKey, maxKey, scanLimit)
		arena := newRowArena(len(cols))
		rows = make([][]interface{}, 0, len(rawRows))
		for _, data := range rawRows {
			row := arena.row()
			decodeRowProjected(meta, data, row, fieldToOut)
			rows = append(rows, row)
		}
	}
	// 超限检查：扫描已在上限处停止，这里给出明确错误（提示加 LIMIT）
	if scanCapped && len(rows) > maxScanRows {
		err = fmt.Errorf("%s: result set exceeds %d rows, add LIMIT to the query "+
			"(raise max_scan_rows if you really need more)", "scan limit", maxScanRows)
		return
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
	for p.peek().kind == kindSym && p.peek().val == ";" {
		p.next()
	}
	if p.peek().kind != kindEof {
		return fmt.Errorf("unexpected %q after statement (unsupported syntax is rejected, not ignored)", p.peek().val)
	}
	return nil
}

// coerceCondLiteral 把条件字面量折算成该列的存储表示：
// DATE/DATETIME → Unix 纳秒，DECIMAL → 放大 10^scale 的定点整数。
// 其余类型原样返回；转换失败也原样返回（由上层比较自然不命中）。
func coerceCondLiteral(f Field, v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch f.Type {
	case TypeDate, TypeDatetime:
		if n, ok := coerceTimeNanos(v, f.Type == TypeDate); ok {
			return n
		}
	case TypeDecimal:
		if n, ok := coerceDecimalScaled(v, f.Len); ok {
			return n
		}
	}
	return v
}

// normalizeGroupCondValues 把 OR / 不可下推路径的条件字面量折算成存储值。
// 这条路径（rowMatchGroupsFlat → evalCond）只看到解码后的 Go 值，没有列类型
// 信息，必须在这里补上折算。
func normalizeGroupCondValues(meta *TableMeta, groups []andGroup) {
	for gi := range groups {
		for ci := range groups[gi] {
			c := &groups[gi][ci]
			fi := meta.fieldIndexOf(c.field)
			if fi < 0 {
				continue
			}
			f := meta.Fields[fi]
			c.val = coerceCondLiteral(f, c.val)
			if c.op == "BETWEEN" {
				c.val2 = coerceCondLiteral(f, c.val2)
			}
		}
	}
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
// （稳定排序保持原顺序），nil 视为最小。
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
	// 整数一律先按整数比较：统一转 float64 会让超过 2^53 的主键
	//（雪花 ID / 纳秒时间戳）比较成"相等"，ORDER BY 静默失效。
	if ai, ok := toIntCell(a); ok {
		if bi, ok2 := toIntCell(b); ok2 {
			switch {
			case ai < bi:
				return -1
			case ai > bi:
				return 1
			}
			return 0
		}
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
		// 与 WHERE 的字符集语义保持一致：ORDER BY 也按 utf8mb4_general_ci
		// 折叠比较，否则会出现"过滤认为相等、排序却把它们排到两端"的矛盾。
		return cmpFold(as, bs)
	}
	return 0
}

// toIntCell 取出整数值（用于排序比较），非整数返回 false。
func toIntCell(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return math.MaxInt64, true
		}
		return int64(n), true
	}
	return 0, false
}

// rowMatchGroupsFlat 按预解析字段下标求值 OR 连接的 AND 条件组（flat 行值）。
// 下标 -1 表示字段不在表中，值视为 nil（与 map 缺键语义一致）。
func rowMatchGroupsFlat(vals []interface{}, groups []andGroup, gIdx [][]int) bool {
	for gi, g := range groups {
		ok := true
		for ci := range g {
			var v interface{}
			if i := gIdx[gi][ci]; i >= 0 {
				v = vals[i]
			}
			if !evalCond(v, &g[ci]) {
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
		// 字符串条件按 utf8mb4_general_ci 折叠：OR / LIKE 等不可下推条件
		// 走的是这条通用谓词路径，语义必须与 scanConds 的字节级内核一致。
		switch c.op {
		case "=":
			return eqFold(vs, bs)
		case "<>", "!=":
			return !eqFold(vs, bs)
		case ">":
			return cmpFold(vs, bs) > 0
		case "<":
			return cmpFold(vs, bs) < 0
		case ">=":
			return cmpFold(vs, bs) >= 0
		case "<=":
			return cmpFold(vs, bs) <= 0
		case "BETWEEN":
			b2, b2ok := c.val2.(string)
			return b2ok && cmpFold(vs, bs) >= 0 && cmpFold(vs, b2) <= 0
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
	n, _ := toInt64OK(v)
	return n
}

// toInt64OK 尝试把条件值转成整数，并返回是否成功。
// 主键范围下推必须用这个版本：旧实现无脑回落 0，`id > '500'` / `id > NULL`
// 会被下推成 `id >= 1` 且不再回验条件，静默返回全表。
func toInt64OK(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
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

// findTopLevelUnion 查找深度 0 的 UNION（不在括号内），返回其下标与是否 ALL。
func findTopLevelUnion(toks []sqlToken) (idx int, all bool, ok bool) {
	depth := 0
	for i, t := range toks {
		if t.kind == kindSym {
			switch t.val {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth == 0 && t.is("UNION") {
			next := i + 1
			isAll := next < len(toks) && toks[next].is("ALL")
			return i, isAll, true
		}
	}
	return 0, false, false
}

// runUnionAs 执行 UNION [ALL] 两侧并合并结果。左侧决定列名。
func (db *DB) runUnionAs(user string, toks []sqlToken, idx int, all bool, sql string, params []interface{}, txnID uint64, schema string) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	eof := sqlToken{kind: kindEof, val: ""}
	left := make([]sqlToken, 0, idx+1)
	left = append(left, toks[:idx]...)
	left = append(left, eof)

	start := idx + 1
	if all {
		start++
	}
	if start >= len(toks) {
		err = fmt.Errorf("expected SELECT after UNION")
		return
	}
	right := append([]sqlToken{}, toks[start:]...)
	if len(right) == 0 || right[len(right)-1].kind != kindEof {
		right = append(right, eof)
	}
	colsL, rowsL, _, _, e1 := db.runSQLTokensAs(user, left, sql, params, txnID, schema)
	if e1 != nil {
		err = e1
		return
	}
	_, rowsR, _, _, e2 := db.runSQLTokensAs(user, right, sql, params, txnID, schema)
	if e2 != nil {
		err = e2
		return
	}
	columns = colsL
	rows = append(rowsL, rowsR...)
	if !all {
		seen := make(map[string]struct{}, len(rows))
		dedup := rows[:0]
		for _, r := range rows {
			key := fmt.Sprint(r)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			dedup = append(dedup, r)
		}
		rows = dedup
	}
	rawMsg = fmt.Sprintf("%d rows in set", len(rows))
	return
}

// findOnDuplicate 在 token 流中查找 ON DUPLICATE KEY UPDATE 的起始下标。
func findOnDuplicate(toks []sqlToken, from int) int {
	for i := from; i+3 < len(toks); i++ {
		if toks[i].is("ON") && toks[i+1].is("DUPLICATE") && toks[i+2].is("KEY") && toks[i+3].is("UPDATE") {
			return i
		}
	}
	return -1
}

// dupAssign ON DUPLICATE KEY UPDATE 的一条赋值。
type dupAssign struct {
	col string
	e   *expr
}

func (db *DB) parseInsert(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	replaceMode := false
	if p.peek().is("REPLACE") {
		p.next()
		replaceMode = true
	} else {
		p.next() // INSERT
	}
	// 修饰符：IGNORE / LOW_PRIORITY / HIGH_PRIORITY / DELAYED
	ignore := false
	for {
		switch {
		case p.peek().is("IGNORE"):
			ignore = true
			p.next()
		case p.peek().is("LOW_PRIORITY"), p.peek().is("HIGH_PRIORITY"), p.peek().is("DELAYED"):
			p.next()
		default:
		}
		if p.peek().is("INTO") {
			break
		}
		if !p.peek().is("IGNORE") && !p.peek().is("LOW_PRIORITY") && !p.peek().is("HIGH_PRIORITY") && !p.peek().is("DELAYED") {
			break
		}
	}
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
	if p.peek().kind == kindSym && p.peek().val == "(" {
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
			if q := p.next(); q.kind != kindSym || q.val != "," {
				err = fmt.Errorf("expected , or )")
				return
			}
		}
	}
	if !p.matchKeyword("VALUES") {
		err = fmt.Errorf("expected VALUES")
		return
	}
	// ON DUPLICATE KEY UPDATE 语法上位于 VALUES 行列表之后，但行循环的冲突
	// 处理需要它，因此先扫描预解析，再回到行列表位置执行。
	var onDup []dupAssign
	onDupParsed := false
	if k := findOnDuplicate(p.toks, p.i); k >= 0 {
		save := p.i
		savedParam := p.paramIdx
		// 预解析会消费 ?：先跳过 VALUES 行列表里的占位符，
		// 让 ON DUPLICATE 的 ? 绑定到正确参数，解析完再把游标回滚。
		for i := save; i < k; i++ {
			if p.toks[i].kind == kindSym && p.toks[i].val == "?" {
				p.paramIdx++
			}
		}
		p.i = k
		p.next() // ON
		p.matchKeyword("DUPLICATE")
		p.matchKeyword("KEY")
		p.matchKeyword("UPDATE")
		for {
			colTok := p.next()
			if colTok.kind != kindIdent && colTok.kind != kindKw {
				p.i = save
				err = fmt.Errorf("expected column in ON DUPLICATE KEY UPDATE")
				return
			}
			if !p.matchSym("=") {
				p.i = save
				err = fmt.Errorf("expected = in ON DUPLICATE KEY UPDATE")
				return
			}
			ae, e := p.parseExpr()
			if e != nil {
				p.i = save
				err = e
				return
			}
			onDup = append(onDup, dupAssign{col: colTok.val, e: ae})
			if !p.matchSym(",") {
				break
			}
		}
		p.i = save // 回到 VALUES 行列表继续执行
		p.paramIdx = savedParam
		onDupParsed = true
	}
	// 多行 INSERT 整体原子：包一个隐式事务，任一行失败即整体回滚
	//（MySQL 语句级原子性）；会话已在事务中（p.txnID != 0）时沿用外层事务。
	// 单行简单 INSERT 走无事务快速路径（batch 模式）：省掉 Begin/Commit、
	// 事务表/Changes map 与读版本记录；主键唯一性仍由 pkTree.SetNX 保证。
	autoTxn := p.txnID == 0
	txnStarted := false
	beginAutoTxn := func() {
		if autoTxn && !txnStarted {
			p.txnID = db.BeginTxn()
			txnStarted = true
		}
	}
	rollback := func(e error) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
		if txnStarted {
			db.RollbackTxn(p.txnID)
			p.txnID = 0
			txnStarted = false
		}
		return nil, nil, 0, "", e
	}
	affected = 0
	// 批量路径的列映射：batchColIdx[i] = 表第 i 个字段在本行 values 里的下标。
	// 适用条件（其余形态继续走逐行 insertRow，语义完全不变）：
	//   - 无 IGNORE / REPLACE / ON DUPLICATE；
	//   - 无显式列清单时列数须与表字段数一致；
	//   - 有显式列清单时必须是"全字段的一个排列"（避免缺省列填充语义与原路径不一致）。
	var batchColIdx []int
	if !ignore && !replaceMode && onDup == nil && len(t.Meta().Fields) > 0 {
		meta := t.Meta()
		if !colsExplicit {
			idx := make([]int, len(meta.Fields))
			for i := range idx {
				idx[i] = i
			}
			batchColIdx = idx
		} else if len(fields) == len(meta.Fields) {
			idx := make([]int, len(meta.Fields))
			for i := range idx {
				idx[i] = -1
			}
			okAll := true
			for vi, f := range fields {
				fi := meta.fieldIndexOf(f)
				if fi < 0 || idx[fi] != -1 {
					okAll = false
					break
				}
				idx[fi] = vi
			}
			if okAll {
				batchColIdx = idx
			}
		}
	}
	pkFieldIdx := 0
	for i, f := range t.Meta().Fields {
		if f.Name == t.Meta().PK {
			pkFieldIdx = i
			break
		}
	}
	var batchPks []int64
	var batchRows [][]interface{}
	for {
		if q := p.next(); q.kind != kindSym || q.val != "(" {
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
			if q := p.next(); q.kind != kindSym || q.val != "," {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				return rollback(fmt.Errorf("expected , or )"))
			}
		}
		// 单行简单 INSERT 且语句已到结尾：batch 模式直接写，不建事务。
		if autoTxn && !txnStarted && !walFsync.Load() && !ignore && !replaceMode && !onDupParsed && p.atSingleRowEnd() {
			p.txnID = 0
		} else {
			beginAutoTxn()
		}
		// 提交单行
		// 简单多行 INSERT（无 IGNORE/REPLACE/ON DUPLICATE、列能完整映射到表字段）
		// 走批量路径：一次主键树锁 + 一条 WAL 记录 + 索引按桶合并。
		if batchColIdx != nil && len(values) == len(batchColIdx) {
			pkPos := batchColIdx[pkFieldIdx]
			var pk int64
			if pkPos >= 0 {
				if v, ok := values[pkPos].(int64); ok && v != 0 {
					pk = v
				}
			}
			if pk == 0 {
				pk = t.allocPK()
				if pkPos >= 0 {
					values[pkPos] = pk
				}
				p.lastInsertID = pk
			}
			// 按表字段序重排（带列清单时就是一次置换）
			ordered := make([]interface{}, len(batchColIdx))
			for i, vi := range batchColIdx {
				ordered[i] = values[vi]
			}
			batchPks = append(batchPks, pk)
			batchRows = append(batchRows, ordered)
			affected++
			if !colsExplicit {
				interfaceSlicePool.Put(values[:0])
			}
			if !p.matchSym(",") {
				break
			}
			if p.peek().kind == kindSym && p.peek().val == "(" {
				continue
			}
			break
		}
		pk, e := db.insertRow(p, t, fields, colsExplicit, values)
		if e != nil {
			if !errors.Is(e, errPKExists) {
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				return rollback(e)
			}
			// 主键冲突：按语句语义分流
			switch {
			case ignore:
				// INSERT IGNORE：跳过该行
			case replaceMode:
				if de := t.Delete(pk, p.txnID, false); de != nil {
					if !colsExplicit {
						interfaceSlicePool.Put(values[:0])
					}
					return rollback(de)
				}
				if _, ie := db.insertRow(p, t, fields, colsExplicit, values); ie != nil {
					if !colsExplicit {
						interfaceSlicePool.Put(values[:0])
					}
					return rollback(ie)
				}
				affected++
			case onDup != nil:
				if ue := db.applyOnDuplicate(p, t, pk, onDup, fields, values); ue != nil {
					if !colsExplicit {
						interfaceSlicePool.Put(values[:0])
					}
					return rollback(ue)
				}
				affected++
			default:
				if !colsExplicit {
					interfaceSlicePool.Put(values[:0])
				}
				return rollback(e)
			}
			if !colsExplicit {
				interfaceSlicePool.Put(values[:0])
			}
			if p.matchSym(",") {
				continue
			}
			break
		}
		if !colsExplicit {
			interfaceSlicePool.Put(values[:0])
		}
		affected++
		// 下一行：逗号后必须是 (；否则结束
		if !p.matchSym(",") {
			break
		}
		if p.peek().kind == kindSym && p.peek().val == "(" {
			continue
		}
		break
	}
	if onDupParsed {
		p.i = len(p.toks) - 1 // ON DUPLICATE 子句已消费
	}
	// 批量收尾：整批一次写入（先整体校验，冲突则整句回滚）
	if len(batchPks) > 0 {
		if e := db.insertBatchOrdered(t, batchPks, batchRows, p.txnID); e != nil {
			return rollback(e)
		}
	}
	if e := p.expectEnd(); e != nil {
		return rollback(e)
	}
	if txnStarted {
		if e := db.CommitTxn(p.txnID); e != nil {
			p.txnID = 0
			txnStarted = false
			return nil, nil, 0, "", e
		}
		p.txnID = 0
		txnStarted = false
	}
	rawMsg = fmt.Sprintf("%d row inserted", affected)
	if affected > 1 {
		rawMsg = fmt.Sprintf("%d rows inserted", affected)
	}
	if p.lastInsertID > 0 {
		// 引擎分配的自增主键：回显给客户端（MySQL 协议另走 OK 包 last insert id）
		rawMsg = fmt.Sprintf("%s (id=%d)", rawMsg, p.lastInsertID)
	}
	return
}

// atSingleRowEnd 判断当前游标后是否只剩可选分号与 EOF，供单行 INSERT 快路径使用。
// 不移动游标；只有能确定语句已完整结束时才允许跳过事务，避免语法错误时已落行。
func (p *sqlParser) atSingleRowEnd() bool {
	i := p.i
	for i < len(p.toks) && p.toks[i].kind == kindSym && p.toks[i].val == ";" {
		i++
	}
	return i < len(p.toks) && p.toks[i].kind == kindEof
}

// insertRow 提交 INSERT 的单行：优先位置参数快速路径，否则按列名映射。
// 事务模式下记录 read-version 与 Changes——CommitTxn 依赖 Changes 写出
// COMMIT 记录并在冲突时回滚，缺失会让重放丢行。
// 返回行的主键（供 INSERT IGNORE / REPLACE / ON DUPLICATE 处理冲突）。
func (db *DB) insertRow(p *sqlParser, t *Table, fields []string, colsExplicit bool, values []interface{}) (int64, error) {
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
			return 0, fmt.Errorf("column count mismatch")
		}
		for i, f := range t.Meta().Fields {
			if f.Name == t.Meta().PK {
				pkIdx = i
				break
			}
		}
	} else {
		if len(fields) != len(values) {
			return 0, fmt.Errorf("column count mismatch")
		}
		for i, f := range fields {
			if f == t.Meta().PK {
				pkIdx = i
				break
			}
		}
	}
	var pk int64
	// autoAssign：主键缺省 / 为 0 / 为 NULL 时由引擎分配（MySQL AUTO_INCREMENT 语义）。
	// 多客户端各自"MAX(pk)+1"播种会撞主键（实测 37% 插入失败），因此交给引擎分配。
	autoAssign := false
	if pkIdx >= 0 && pkIdx < len(values) {
		switch v := values[pkIdx].(type) {
		case int64:
			if v == 0 {
				autoAssign = true
			} else {
				pk = v
			}
		case nil:
			autoAssign = true
		default:
			return 0, fmt.Errorf("primary key must be integer")
		}
	} else {
		// 主键缺省：由引擎分配（MySQL AUTO_INCREMENT 语义）。
		autoAssign = true
	}
	if autoAssign {
		pk = t.allocPK()
		p.lastInsertID = pk
		if orderedPath && pkIdx >= 0 && pkIdx < len(values) {
			values[pkIdx] = pk
		}
	}
	if orderedPath {
		// 位置参数快速路径：不构建每行 map，也不按列名回查。
		if err := db.txnApply(p.txnID, t, pk, func() error {
			return t.InsertOrdered(pk, values, 0, p.txnID, false)
		}); err != nil {
			return pk, err
		}
		return pk, nil
	}
	// 显式列清单且与表字段序不一致：保留按列名映射路径。
	rowMap := make(map[string]interface{}, len(fields))
	for i, f := range fields {
		rowMap[f] = values[i]
	}
	if autoAssign {
		rowMap[t.Meta().PK] = pk
	}
	if err := db.txnApply(p.txnID, t, pk, func() error {
		return t.Insert(pk, rowMap, 0, p.txnID, false)
	}); err != nil {
		return pk, err
	}
	return pk, nil
}

// applyOnDuplicate 执行 ON DUPLICATE KEY UPDATE：在既有行上求值赋值表达式。
// 表达式可引用既有列与 VALUES(col)（本次待插入的值）。
func (db *DB) applyOnDuplicate(p *sqlParser, t *Table, pk int64, assigns []dupAssign, fields []string, values []interface{}) error {
	cur, ok := t.pkTree.Get(pk)
	if !ok {
		return fmt.Errorf("row not found")
	}
	row, _, _ := decodeRow(t.Meta(), cur)
	// VALUES(col) → 合成列 __values_<col>
	evalRow := make(map[string]interface{}, len(row)+len(fields))
	for k, v := range row {
		evalRow[k] = v
	}
	cols := fields
	if len(cols) == 0 {
		// 位置参数形式（无显式列清单）：列序即表字段序
		for _, f := range t.Meta().Fields {
			cols = append(cols, f.Name)
		}
	}
	for i, f := range cols {
		if i < len(values) {
			evalRow[lowerKey("__values_"+f)] = values[i]
		}
	}
	for _, a := range assigns {
		row[a.col] = rewriteValuesFunc(a.e).eval(evalRow)
	}
	return db.txnApply(p.txnID, t, pk, func() error {
		return t.Update(pk, row, 0, p.txnID, false)
	})
}

// rewriteValuesFunc 把 VALUES(col) 改写成对合成列的引用。
func rewriteValuesFunc(e *expr) *expr {
	if e == nil {
		return nil
	}
	if e.kind == exFunc && e.fn == "VALUES" && len(e.args) == 1 && e.args[0].kind == exCol {
		return &expr{kind: exCol, col: "__values_" + e.args[0].col, raw: "VALUES(" + e.args[0].col + ")"}
	}
	out := *e
	out.args = mapExprSlice(e.args, rewriteValuesFunc)
	out.when = mapExprSlice(e.when, rewriteValuesFunc)
	out.els = rewriteValuesFunc(e.els)
	return &out
}

// 返回索引首列与索引名（未命名时用首列名）。不是索引定义则消费完并返回 ok=false。
func readTableIndexDef(p *sqlParser) (col, name string, ok bool) {
	isIndex := false
	for {
		t := p.peek()
		switch {
		case t.is("UNIQUE"), t.is("FULLTEXT"), t.is("SPATIAL"):
			p.next()
		case t.is("KEY"), t.is("INDEX"):
			p.next()
			isIndex = true
		default:
		}
		if isIndex || !(p.peek().is("UNIQUE") || p.peek().is("FULLTEXT") || p.peek().is("SPATIAL")) {
			break
		}
	}
	if !isIndex {
		consumeTableLevelConstraint(p)
		return "", "", false
	}
	if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
		if !(n.is("USING") || n.is("ON") || n.kind == kindSym) {
			name = n.val
			p.next()
		}
	}
	c, got := readIndexColumns(p)
	// 尾部（USING BTREE / COMMENT ...）交给消费函数
	for {
		t := p.peek()
		if t.kind == kindEof || (t.kind == kindSym && (t.val == "," || t.val == ")")) {
			break
		}
		p.next()
	}
	if !got {
		return "", "", false
	}
	if name == "" {
		name = c
	}
	return c, name, true
}

// readIndexColumns 读取索引列清单 "(col[, ...])"，返回第一列。
func readIndexColumns(p *sqlParser) (string, bool) {
	if !(p.peek().kind == kindSym && p.peek().val == "(") {
		return "", false
	}
	p.next()
	first := ""
	for {
		t := p.next()
		if t.kind == kindIdent || t.kind == kindKw {
			if first == "" {
				first = t.val
			}
		}
		if p.matchSym(")") {
			break
		}
		if !p.matchSym(",") {
			// 异常形态：跳过到 )
			for !p.peek().is(")") && p.peek().kind != kindEof {
				p.next()
			}
			p.matchSym(")")
			break
		}
	}
	return first, first != ""
}

// isTableLevelConstraint 判断 token 是否是表级约束/索引关键字。
func isTableLevelConstraint(t sqlToken) bool {
	if t.kind != kindKw && t.kind != kindIdent {
		return false
	}
	switch strings.ToUpper(t.val) {
	case "KEY", "INDEX", "UNIQUE", "FULLTEXT", "SPATIAL", "CONSTRAINT", "FOREIGN", "CHECK":
		return true
	}
	return false
}

// consumeTableLevelConstraint 吃掉一条表级约束/索引定义（含可选的 (cols)）。
func consumeTableLevelConstraint(p *sqlParser) {
	p.next() // 关键字
	switch {
	default:
	}
	if p.peek().is("KEY") || p.peek().is("INDEX") {
		p.next()
	}
	if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
		p.next()
	}
	skipOptionalParenList(p)
	// REFERENCES ... / CHECK (...) 等尾巴
	for {
		t := p.peek()
		if t.kind == kindEof {
			return
		}
		if t.kind == kindSym && (t.val == "," || t.val == ")") {
			return
		}
		if t.kind == kindSym && t.val == "(" {
			p.next()
			skipToParen(p)
			continue
		}
		p.next()
	}
}

// skipOptionalParenList 跳过可选的 "(a,b)" 列表（索引列清单等）。
func skipOptionalParenList(p *sqlParser) {
	if p.peek().kind == kindSym && p.peek().val == "(" {
		p.next()
		skipToParen(p)
	}
}

// skipToParen 跳过直到配对的 ")"（当前已消费 "("）。
func skipToParen(p *sqlParser) {
	depth := 1
	for {
		t := p.peek()
		if t.kind == kindEof {
			return
		}
		if t.kind == kindSym && t.val == "(" {
			depth++
		}
		if t.kind == kindSym && t.val == ")" {
			depth--
			p.next()
			if depth == 0 {
				return
			}
			continue
		}
		p.next()
	}
}

// parseColumnType 解析 "类型[(参数[,参数])]"；三个 DDL 分支（CREATE /
// ALTER MODIFY / ALTER ADD）共用，避免 DECIMAL(p,s) 这类双参数类型各写一遍。
//
// 返回值 flen 的含义随类型不同：
//   - VARCHAR(len)：显示长度（不参与行编码，上限由 maxVarcharLen 决定）；
//   - DECIMAL(p,s)：**scale s**（存进 Field.Len，行内是放大 10^s 的定点整数）。
//     精度 p 只做合法性校验（p-s ≤ 18，s ≤ decimalMaxScale），不落盘；
//   - 其它类型：0。
func (p *sqlParser) parseColumnType() (FieldType, int, error) {
	typeTok := p.next()
	if typeTok.kind != kindKw && typeTok.kind != kindIdent {
		return 0, 0, fmt.Errorf("expected column type")
	}
	ftype, err := parseFieldType(typeTok.val)
	if err != nil {
		return 0, 0, err
	}
	flen := 0
	if ftype == TypeVarchar {
		flen = 255
	}
	q := p.next()
	if q.kind != kindSym || q.val != "(" {
		p.i-- // 回退已读符号
		return ftype, flen, nil
	}
	num := p.next()
	if num.kind != kindNum {
		return 0, 0, fmt.Errorf("expected length")
	}
	precision, _ := strconv.Atoi(num.val)
	if ftype == TypeDecimal {
		scale := 0
		if p.matchSym(",") {
			n2 := p.next()
			if n2.kind != kindNum {
				return 0, 0, fmt.Errorf("expected DECIMAL scale")
			}
			scale, _ = strconv.Atoi(n2.val)
		}
		if scale < 0 || scale > decimalMaxScale {
			return 0, 0, fmt.Errorf("DECIMAL scale %d out of range (0..%d)", scale, decimalMaxScale)
		}
		if precision <= 0 || precision-scale > 18 {
			return 0, 0, fmt.Errorf("DECIMAL(%d,%d) out of range: integer digits must be 1..18", precision, scale)
		}
		flen = scale
	} else {
		flen = precision
	}
	if q := p.next(); q.kind != kindSym || q.val != ")" {
		return 0, 0, fmt.Errorf("expected )")
	}
	return ftype, flen, nil
}

// parseCreateTable 解析 CREATE TABLE ...。
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
	if q := p.next(); q.kind != kindSym || q.val != "(" {
		err = fmt.Errorf("expected (")
		return
	}
	var fields []Field
	var pkName string
	pkNameSet := false
	// 表级索引定义（KEY/INDEX [name] (col)）：真实注册进 meta.Indexes
	var tableIndexes map[string]string
	for {
		// 分隔符与语句结束统一在循环开头处理
		if p.peek().kind == kindSym && p.peek().val == "," {
			p.next()
			continue
		}
		if p.peek().kind == kindSym && p.peek().val == ")" {
			p.next()
			break
		}
		if p.peek().kind == kindEof {
			err = fmt.Errorf("expected )")
			return
		}
		// 表级约束/索引定义：KEY/INDEX/UNIQUE/FULLTEXT/SPATIAL/CONSTRAINT/FOREIGN/CHECK
		if t := p.peek(); isTableLevelConstraint(t) {
			// KEY/INDEX/UNIQUE 定义要真实注册；其余（CONSTRAINT/FOREIGN/CHECK）跳过
			if col, name, ok := readTableIndexDef(p); ok {
				if tableIndexes == nil {
					tableIndexes = map[string]string{}
				}
				tableIndexes[name] = col
			}
			continue
		}
		// 允许 PRIMARY KEY (col)
		if p.matchKeyword("PRIMARY") {
			if !p.matchKeyword("KEY") {
				err = fmt.Errorf("expected KEY")
				return
			}
			if q := p.next(); q.kind != kindSym || q.val != "(" {
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
			// 复合主键只取第一列（存储层仅支持单列主键）
			for p.matchSym(",") {
				if _, e := p.parseIdent(); e != nil {
					err = e
					return
				}
			}
			if !p.matchSym(")") {
				err = fmt.Errorf("expected )")
				return
			}
			continue
		}
		fname, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		ftype, flen, e := p.parseColumnType()
		if e != nil {
			err = e
			return
		}
		// 行内 PRIMARY KEY / 其它列属性（NOT NULL、DEFAULT、AUTO_INCREMENT、
		// UNIQUE、KEY、COMMENT、COLLATE、CHARACTER SET、ON UPDATE ...）一律容忍并跳过，
		// 只有 PRIMARY KEY 会生效（存储层没有约束引擎，见 README「已知限制」）。
		for {
			t := p.peek()
			if t.kind == kindEof {
				break
			}
			if t.kind == kindSym && (t.val == "," || t.val == ")") {
				break
			}
			switch strings.ToUpper(t.val) {
			case "PRIMARY":
				p.next()
				p.matchKeyword("KEY")
				if !pkNameSet {
					pkName = fname
					pkNameSet = true
				}
			case "UNIQUE":
				p.next()
				p.matchKeyword("KEY")
				p.matchKeyword("INDEX")
				skipOptionalParenList(p)
			case "KEY", "INDEX":
				p.next()
				if t2 := p.peek(); t2.kind == kindIdent || t2.kind == kindKw {
					p.next()
				}
				skipOptionalParenList(p)
			case "NOT":
				p.next()
				p.matchKeyword("NULL")
			case "NULL":
				p.next()
			case "DEFAULT":
				p.next()
				if p.peek().kind == kindSym && p.peek().val == "(" {
					p.next()
					skipToParen(p)
				} else {
					p.next()
				}
			case "AUTO_INCREMENT":
				p.next()
			case "COMMENT":
				p.next()
				p.next()
			case "COLLATE", "CHARACTER":
				p.next()
				if p.peek().is("SET") {
					p.next()
				}
				p.next()
			case "ON":
				p.next()
				p.matchKeyword("UPDATE")
				p.next()
			case "REFERENCES", "CHECK", "CONSTRAINT", "FOREIGN", "SPATIAL", "FULLTEXT":
				for {
					t2 := p.peek()
					if t2.kind == kindEof {
						break
					}
					if t2.kind == kindSym && (t2.val == "," || t2.val == ")") {
						break
					}
					p.next()
				}
			default:
				p.next()
			}
		}
		fields = append(fields, Field{Name: fname, Type: ftype, Len: flen})
		continue
	}
	if !pkNameSet {
		err = fmt.Errorf("table must have a PRIMARY KEY")
		return
	}
	// 表级索引定义：KEY/INDEX [name] (col[, ...])
	for {
		t := p.peek()
		if t.kind != kindKw && t.kind != kindIdent {
			break
		}
		switch strings.ToUpper(t.val) {
		case "KEY", "INDEX":
			p.next()
			idxName := ""
			if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
				idxName = n.val
				p.next()
			}
			if col, ok := readIndexColumns(p); ok && idxName != "" {
				if tableIndexes == nil {
					tableIndexes = map[string]string{}
				}
				tableIndexes[idxName] = col
			} else {
				skipOptionalParenList(p)
			}
		case "UNIQUE":
			p.next()
			p.matchKeyword("KEY")
			p.matchKeyword("INDEX")
			idxName := ""
			if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
				idxName = n.val
				p.next()
			}
			if col, ok := readIndexColumns(p); ok && idxName != "" {
				if tableIndexes == nil {
					tableIndexes = map[string]string{}
				}
				tableIndexes[idxName] = col
			} else {
				skipOptionalParenList(p)
			}
		case "FULLTEXT", "SPATIAL":
			p.next()
			p.matchKeyword("KEY")
			p.matchKeyword("INDEX")
			if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
				p.next()
			}
			skipOptionalParenList(p)
		case "CONSTRAINT", "FOREIGN", "CHECK":
			for {
				t2 := p.peek()
				if t2.kind == kindEof {
					break
				}
				if t2.kind == kindSym && (t2.val == "," || t2.val == ")") {
					break
				}
				p.next()
			}
		default:
			// 引擎/字符集等表选项：KEY=VALUE 或单关键字（DEFAULT CHARSET=utf8mb4 等）
			p.next()
			if p.peek().kind == kindSym && p.peek().val == "=" {
				p.next()
				p.next()
			}
		}
		// 选项之间可以没有逗号（ENGINE=InnoDB DEFAULT CHARSET=utf8mb4）
		if p.peek().kind == kindEof {
			break
		}
		if p.peek().kind == kindSym && p.peek().val == ")" {
			break
		}
		if !p.matchSym(",") {
			continue
		}
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	if tableIndexes == nil {
		tableIndexes = map[string]string{}
	}
	meta := &TableMeta{
		Name:    name,
		PK:      pkName,
		Fields:  fields,
		Indexes: tableIndexes,
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
	if err := appendWAL(buf, false); err != nil {
		db.mu.Unlock()
		return err
	}
	db.tables.Delete(name)
	db.tablesCount.Add(-1)
	delete(db.catalog, name)
	walRegUnregister(name)
	db.mu.Unlock()
	// 授权记录以表名为键：不清理会让同名重建表自动继承旧授权
	if err := db.privileges.DropTable(name); err != nil {
		logf(LOG_WARN, "drop privileges for %s: %v", name, err)
	}
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
	if p.peek().is("DROP") {
		p.next()
		if p.matchKeyword("INDEX") || p.matchKeyword("KEY") {
			idxName, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			if e := p.expectEnd(); e != nil {
				err = e
				return
			}
			if e := db.AlterTableDropIndex(name, idxName); e != nil {
				err = e
				return
			}
			rawMsg = fmt.Sprintf("index %s dropped", idxName)
			affected = 1
			return
		}
		if p.matchKeyword("PRIMARY") {
			p.matchKeyword("KEY")
			err = fmt.Errorf("dropping the primary key is not supported")
			return
		}
		p.matchKeyword("COLUMN")
		col, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		p.matchKeyword("RESTRICT")
		p.matchKeyword("CASCADE")
		if e := p.expectEnd(); e != nil {
			err = e
			return
		}
		if e := db.AlterTableDropColumn(name, col); e != nil {
			err = e
			return
		}
		rawMsg = fmt.Sprintf("column %s dropped", col)
		affected = 1
		return
	}
	if p.peek().is("MODIFY") || p.peek().is("CHANGE") {
		isChange := p.peek().is("CHANGE")
		p.next()
		p.matchKeyword("COLUMN")
		firstCol, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		target := firstCol
		if isChange {
			// CHANGE [COLUMN] old new TYPE
			newCol, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			target = newCol
			if newCol != firstCol {
				if e := db.AlterTableRenameColumn(name, firstCol, newCol); e != nil {
					err = e
					return
				}
			}
		}
		ftype, flen, e := p.parseColumnType()
		if e != nil {
			err = e
			return
		}
		// 跳过列属性（NOT NULL / DEFAULT ... 等）
		for {
			t := p.peek()
			if t.kind == kindEof {
				break
			}
			if t.kind == kindSym && (t.val == "," || t.val == ")") {
				break
			}
			p.next()
		}
		if e := p.expectEnd(); e != nil {
			err = e
			return
		}
		if e := db.AlterTableModifyColumn(name, target, ftype, flen); e != nil {
			err = e
			return
		}
		rawMsg = fmt.Sprintf("column %s modified", target)
		affected = 1
		return
	}
	if p.matchKeyword("ADD") {
		// ADD [UNIQUE] [INDEX|KEY] [name] (col)
		if p.peek().is("UNIQUE") || p.peek().is("INDEX") || p.peek().is("KEY") || p.peek().is("FULLTEXT") || p.peek().is("SPATIAL") {
			for p.peek().is("UNIQUE") || p.peek().is("FULLTEXT") || p.peek().is("SPATIAL") {
				p.next()
			}
			p.matchKeyword("INDEX")
			p.matchKeyword("KEY")
			idxName := ""
			if n := p.peek(); n.kind == kindIdent || n.kind == kindKw {
				idxName = n.val
				p.next()
			}
			if !p.matchSym("(") {
				err = fmt.Errorf("expected ( after index name")
				return
			}
			col, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			for p.matchSym(",") {
				if _, e := p.parseIdent(); e != nil {
					err = e
					return
				}
			}
			if !p.matchSym(")") {
				err = fmt.Errorf("expected )")
				return
			}
			if idxName == "" {
				idxName = col
			}
			if e := p.expectEnd(); e != nil {
				err = e
				return
			}
			if e := db.AlterTableAddIndex(name, idxName, col); e != nil {
				err = e
				return
			}
			rawMsg = fmt.Sprintf("index %s added", idxName)
			affected = 1
			return
		}
		if !p.matchKeyword("COLUMN") {
			err = fmt.Errorf("expected COLUMN")
			return
		}
		fname, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		ftype, flen, e := p.parseColumnType()
		if e != nil {
			err = e
			return
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
		// RENAME COLUMN old TO new
		if p.matchKeyword("COLUMN") {
			oldCol, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			if !p.matchKeyword("TO") {
				err = fmt.Errorf("expected TO")
				return
			}
			newCol, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			if e := p.expectEnd(); e != nil {
				err = e
				return
			}
			if e := db.AlterTableRenameColumn(name, oldCol, newCol); e != nil {
				err = e
				return
			}
			rawMsg = fmt.Sprintf("column %s renamed to %s", oldCol, newCol)
			affected = 1
			return
		}
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

// hasPrefixFold 大小写不敏感的前缀比较（标准库无此函数；避免为每次检查
// 做一次整串 ToLower/ToUpper 拷贝）。
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// matchAnyFold 报告 w 与 words 中任一词大小写不敏感相等（零分配）。
func matchAnyFold(w string, words ...string) bool {
	for _, x := range words {
		if strings.EqualFold(w, x) {
			return true
		}
	}
	return false
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
