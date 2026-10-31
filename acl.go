package main

import (
	"fmt"
	"net/http"
	"strings"
)

// ==================== 数据访问控制（ACL） ====================
// 非管理员用户可通过用户管理配置可用的数据库列表（Databases，空=全部）
// 与可用的表列表（Tables，空=下列数据库中的全部表），本文件在所有
// 数据管理接口处统一校验。管理员始终拥有全部权限。
//
// 设计要点：权限判定必须建立在**已分词的语义**上，而不是字符串首词猜测。
// 旧实现用 firstWord 判断语句类型、用少量关键字猜表名，导致 `;DROP ...`、
// `SELECT * FROM 't'` 之类的写法可以绕过白名单与表级授权。现在统一走
// analyzeSQL → (语句类型, 表引用列表)，未知语句默认拒绝（fail-closed）。

// currentUser 从请求还原登录用户。
func currentUser(r *http.Request) *User {
	name := requireUser(r)
	if name == "" {
		return nil
	}
	return globalUsers.Get(name)
}

// fullAccess 判断用户是否为管理员。
func (u *User) fullAccess() bool {
	return u != nil && u.IsAdmin
}

// allowedDBs 返回用户可见的数据库列表；返回 nil 表示不限制（全部）。
func (u *User) allowedDBs() []string {
	if u.fullAccess() || len(u.Databases) == 0 {
		return nil
	}
	return u.Databases
}

// dbOfTable 从物理表名推断所属数据库：带 "db." 前缀用前缀，否则归入 tsumugi。
func dbOfTable(table string) string {
	if i := strings.IndexByte(table, '.'); i >= 0 {
		return table[:i]
	}
	return "tsumugi"
}

// canUseDB 判断是否允许访问指定数据库。nil 用户（令牌残留等）一律拒绝，
// 避免空指针 panic（用户被删除后令牌仍在请求中属于正常时序）。
func (u *User) canUseDB(dbname string) bool {
	if u == nil {
		return false
	}
	if u.fullAccess() || len(u.Databases) == 0 {
		return true
	}
	for _, d := range u.Databases {
		if d == dbname {
			return true
		}
	}
	return false
}

// canUseTable 判断是否允许访问指定物理表（含 db. 前缀的名称）。
func (u *User) canUseTable(table string) bool {
	if u == nil {
		return false
	}
	if u.fullAccess() {
		return true
	}
	// 系统表（__tables / __indexes / __stats）只对管理员开放
	if strings.HasPrefix(table, "__") {
		return false
	}
	if !u.canUseDB(dbOfTable(table)) {
		return false
	}
	if len(u.Tables) == 0 {
		return true
	}
	for _, t := range u.Tables {
		if t == table {
			return true
		}
	}
	return false
}

// filterDBList 按用户权限过滤数据库列表；用户全权限时原样返回。
func filterDBList(list []string, u *User) []string {
	if u == nil {
		return nil
	}
	if u.fullAccess() {
		return list
	}
	allow := make(map[string]bool, len(u.Databases))
	for _, d := range u.Databases {
		allow[d] = true
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		if allow[d] {
			out = append(out, d)
		}
	}
	return out
}

// filterTables 按用户权限过滤表列表；返回子集。
func filterTables(tables []map[string]interface{}, u *User) []map[string]interface{} {
	if u == nil {
		return nil
	}
	if u.fullAccess() {
		return tables
	}
	out := make([]map[string]interface{}, 0, len(tables))
	for _, tb := range tables {
		name, _ := tb["name"].(string)
		if u.canUseTable(name) {
			out = append(out, tb)
		}
	}
	return out
}

// ==================== 语句语义分析 ====================

// sqlStmtKind 语句类别，用于权限映射与白名单。
type sqlStmtKind int

const (
	sqlUnknown sqlStmtKind = iota
	sqlRead                // SELECT / SHOW / DESCRIBE / EXPLAIN
	sqlInsert
	sqlUpdate
	sqlDelete
	sqlDDL // CREATE / DROP / ALTER / RENAME / TRUNCATE
	sqlTxn // BEGIN / COMMIT / ROLLBACK / SET
	sqlUse // USE <db>
	sqlGrant
)

func (k sqlStmtKind) String() string {
	switch k {
	case sqlRead:
		return "SELECT"
	case sqlInsert:
		return "INSERT"
	case sqlUpdate:
		return "UPDATE"
	case sqlDelete:
		return "DELETE"
	case sqlDDL:
		return "DDL"
	case sqlTxn:
		return "TRANSACTION"
	case sqlUse:
		return "USE"
	case sqlGrant:
		return "GRANT"
	}
	return "UNKNOWN"
}

// perm 把语句类别映射为所需权限位（DDL/GRANT 用 PermDDL 表示结构权限）。
func (k sqlStmtKind) perm() byte {
	switch k {
	case sqlRead:
		return PermSelect
	case sqlInsert:
		return PermInsert
	case sqlUpdate:
		return PermUpdate
	case sqlDelete:
		return PermDelete
	case sqlDDL, sqlGrant:
		return PermDDL
	}
	return PermAll
}

// sqlColumnRef 是 SELECT 投影里的一个列引用。Table 为空表示未限定（`col`），
// 非空表示限定写法（`t.col` / `db.t.col`，取最后两段）。
type sqlColumnRef struct {
	Table  string
	Column string
}

// sqlAccess 是一条语句的权限画像。
type sqlAccess struct {
	Kind   sqlStmtKind
	Tables []string // 引用到的物理表（保留原始大小写，比较时统一小写）
	DB     string   // USE 的目标库 / 库级 DDL 的库名
	// SELECT 投影里引用到的列（仅 sqlRead 且首词为 SELECT 时填充），
	// 用于列级读权限校验。Star=true 表示投影中出现裸 `*` 或 `t.*`。
	Columns []sqlColumnRef
	Star    bool
}

// analyzeSQL 对原始 SQL 做权限相关分析。解析失败返回 error（调用方应拒绝）。
func analyzeSQL(sql string) (sqlAccess, error) {
	toks, err := tokenizeSQL(sql)
	if err != nil {
		return sqlAccess{}, err
	}
	defer putSQLTokens(toks)
	return analyzeSQLTokens(toks)
}

// analyzeSQLTokens 已分词版本（prepared statement 复用缓存 token）。
func analyzeSQLTokens(toks []sqlToken) (sqlAccess, error) {
	var a sqlAccess
	// 跳过前导分号与空语句：`;DROP TABLE t` 必须被识别成 DROP 而不是未知语句
	i := 0
	for i < len(toks) && toks[i].kind == kindSym && toks[i].val == ";" {
		i++
	}
	if i >= len(toks) || toks[i].kind == kindEof {
		return a, fmt.Errorf("empty query")
	}
	head := toks[i]
	if head.kind != kindKw && head.kind != kindIdent {
		return a, fmt.Errorf("unrecognized statement")
	}
	switch tokUp(head) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN":
		a.Kind = sqlRead
	case "INSERT", "REPLACE":
		a.Kind = sqlInsert
	case "UPDATE":
		a.Kind = sqlUpdate
	case "DELETE":
		a.Kind = sqlDelete
	case "CREATE", "DROP", "ALTER", "RENAME", "TRUNCATE":
		a.Kind = sqlDDL
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SET", "SAVEPOINT", "RELEASE":
		a.Kind = sqlTxn
	case "USE":
		a.Kind = sqlUse
	case "GRANT", "REVOKE":
		a.Kind = sqlGrant
	default:
		return a, fmt.Errorf("unsupported statement: %s", head.val)
	}

	a.Tables = extractTableRefs(toks, i, a.Kind, tokUp(head))
	// SELECT 投影列：列级权限校验的输入。只对首词为 SELECT 的读语句填充
	//（SHOW / DESCRIBE / EXPLAIN 不返回行数据，不做列级限制）。
	if a.Kind == sqlRead && tokUp(head) == "SELECT" {
		a.Columns, a.Star = extractSelectProjection(toks, i)
	}
	// USE / CREATE DATABASE / DROP DATABASE：取库名
	if a.Kind == sqlUse {
		if j := i + 1; j < len(toks) && isNameToken(toks[j]) {
			a.DB = toks[j].val
		}
		return a, nil
	}
	if a.Kind == sqlDDL {
		for j := i + 1; j < len(toks) && j < i+4; j++ {
			if tokUp(toks[j]) == "DATABASE" || tokUp(toks[j]) == "SCHEMA" {
				for k := j + 1; k < len(toks); k++ {
					if tokUp(toks[k]) == "IF" || tokUp(toks[k]) == "NOT" || tokUp(toks[k]) == "EXISTS" {
						continue
					}
					if isNameToken(toks[k]) {
						a.DB = toks[k].val
					}
					break
				}
				break
			}
		}
	}
	// 单表语句必须有表名，否则视为解析不到位 → 拒绝（避免"提取不到就放行"）
	// SELECT 允许无表（SELECT 1 / @@version），不强制。
	if a.Kind == sqlInsert || a.Kind == sqlUpdate || a.Kind == sqlDelete || a.Kind == sqlDDL {
		if len(a.Tables) == 0 && a.DB == "" {
			return a, fmt.Errorf("cannot determine target table of %s statement", a.Kind)
		}
	}
	return a, nil
}

// tokUp 返回 token 的大写形式。分词器只为关键字预计算 upper，标识符
// （如 DATABASE / GRANT，未被列入关键字表）需要现算——ACL 属冷路径，可接受。
func tokUp(t sqlToken) string {
	if t.upper != "" {
		return t.upper
	}
	return strings.ToUpper(t.val)
}

// isNameToken 判断 token 是否可作为标识符/表名。字符串字面量也算：
// 解析器允许 'tbl' 当表名，ACL 若不认就会漏判（P0-3 的成因）。
func isNameToken(t sqlToken) bool {
	return t.kind == kindIdent || t.kind == kindKw || t.kind == kindStr
}

// extractTableRefs 从 token 流中提取被引用的表名。
// 覆盖：FROM / JOIN / INTO / UPDATE / DELETE FROM / TABLE / TRUNCATE /
// DESCRIBE / DESC / CREATE INDEX ... ON / GRANT ... ON / RENAME ... TO。
func extractTableRefs(toks []sqlToken, start int, kind sqlStmtKind, head string) []string {
	var names []string
	seen := map[string]bool{}
	addName := func(j int) int {
		if j >= len(toks) || !isNameToken(toks[j]) {
			return j
		}
		name := toks[j].val
		// db.table 形式的限定名
		if j+2 < len(toks) && toks[j+1].kind == kindSym && toks[j+1].val == "." && isNameToken(toks[j+2]) {
			name += "." + toks[j+2].val
			j += 2
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		return j
	}
	// addNameList 解析逗号分隔的表名列表（FROM a, b）。
	// 只处理 `name [AS alias] [, ...]` 的第一段名字，别名与后续列名不影响判定。
	addNameList := func(j int) int {
		for {
			j = addName(j)
			if j+1 < len(toks) && toks[j+1].kind == kindSym && toks[j+1].val == "," {
				j += 2
				continue
			}
			return j
		}
	}
	// 修饰词：IF [NOT] EXISTS / TABLE / LOW_PRIORITY / IGNORE / ONLY
	skipModifiers := func(j int) int {
		for j < len(toks) {
			u := tokUp(toks[j])
			if u == "IF" || u == "NOT" || u == "EXISTS" || u == "LOW_PRIORITY" ||
				u == "HIGH_PRIORITY" || u == "IGNORE" || u == "ONLY" || u == "QUICK" ||
				u == "DELAYED" || u == "CONCURRENTLY" {
				j++
				continue
			}
			return j
		}
		return j
	}
	for j := start; j < len(toks); j++ {
		t := toks[j]
		if t.kind == kindEof {
			break
		}
		if t.kind != kindKw && t.kind != kindIdent {
			continue
		}
		switch tokUp(t) {
		case "FROM":
			// SHOW TABLES FROM db / SHOW DATABASES 里的 FROM 指向库而不是表，跳过
			if head == "SHOW" {
				continue
			}
			j = addNameList(skipModifiers(j+1)) - 1
		case "JOIN", "INTO", "UPDATE", "TRUNCATE", "DESCRIBE", "DESC":
			// DELETE FROM / INSERT INTO / SELECT ... FROM 均在此命中
			j = addName(skipModifiers(j+1)) - 1
		case "TABLE":
			// CREATE/DROP/ALTER TABLE [IF [NOT] EXISTS] name；RENAME TABLE a TO b 由 TO 分支处理
			j = addName(skipModifiers(j+1)) - 1
		case "ON":
			// 仅 CREATE INDEX ... ON t / GRANT ... ON t 需要，避免 JOIN ... ON 误判
			if kind == sqlDDL || kind == sqlGrant {
				j = addName(skipModifiers(j+1)) - 1
			}
		case "TO":
			if kind == sqlDDL { // RENAME TABLE old TO new
				j = addName(skipModifiers(j+1)) - 1
			}
		}
	}
	return names
}

// extractSelectProjection 从 SELECT 的投影段（SELECT 之后、顶层 FROM 之前）
// 提取列引用，供列级权限校验。
//
// 规则（宁可多判、不可漏判）：
//   - 顶层裸 `*` / `t.*` → star=true（配置了列白名单时整条 SELECT 被拒）；
//   - 函数名（紧跟 "(" 的标识符）不算列；函数参数里的标识符仍算列
//     （COUNT(*) 不算列，COUNT(DISTINCT secret) 会命中 secret）；
//   - `AS alias` 里的别名不算列；DISTINCT / ALL / TRUE / FALSE / NULL 跳过；
//   - 限定名 a.b 只取 b（a 视为表名/别名），并把 a 记录到 Table 供多表判断。
func extractSelectProjection(toks []sqlToken, start int) (cols []sqlColumnRef, star bool) {
	depth := 0
	skipAlias := false
	for j := start + 1; j < len(toks); j++ {
		t := toks[j]
		if t.kind == kindEof {
			break
		}
		if t.kind == kindSym {
			switch t.val {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case "*":
				// 只有顶层 * 才是"全部列"；COUNT(*) 里的 * 在括号内，不算。
				if depth == 0 {
					star = true
				}
			case ",":
				if depth == 0 {
					skipAlias = false
				}
			}
			continue
		}
		// 顶层 FROM 之后是表引用 / WHERE / ORDER BY，不再属于投影。
		if depth == 0 && (t.kind == kindKw || t.kind == kindIdent) && tokUp(t) == "FROM" {
			break
		}
		if skipAlias {
			skipAlias = false
			continue
		}
		if t.kind != kindKw && t.kind != kindIdent && t.kind != kindStr {
			continue
		}
		switch u := tokUp(t); u {
		case "DISTINCT", "ALL":
			continue
		case "AS":
			skipAlias = true
			continue
		case "NULL", "TRUE", "FALSE":
			continue
		}
		// 函数名：紧跟 "(" 的标识符不是列。
		if j+1 < len(toks) && toks[j+1].kind == kindSym && toks[j+1].val == "(" {
			continue
		}
		// 限定名的前半段（后面紧跟 "."）是表名/别名，真正的列在后面。
		if j+1 < len(toks) && toks[j+1].kind == kindSym && toks[j+1].val == "." {
			continue
		}
		ref := sqlColumnRef{Column: t.val}
		if j >= 2 && toks[j-1].kind == kindSym && toks[j-1].val == "." && isNameToken(toks[j-2]) {
			ref.Table = toks[j-2].val
		}
		cols = append(cols, ref)
	}
	return cols, star
}

// ==================== Web 控制台策略 ====================

// canExecSQL 权限引擎：非管理员只允许只读语句，写/DDL 一律拒绝。
// 未知语句默认拒绝（旧实现在 default 分支放行）。
func (u *User) canExecSQL(sql string) (bool, string) {
	if u == nil {
		return false, "permission denied: not authenticated"
	}
	if u.fullAccess() {
		return true, ""
	}
	a, err := analyzeSQL(sql)
	if err != nil {
		return false, "permission denied: " + err.Error()
	}
	switch a.Kind {
	case sqlRead, sqlUse:
		return true, ""
	case sqlTxn:
		return false, "permission denied: transactions are admin-only in the web console"
	default:
		return false, "permission denied: write or DDL statements are admin-only"
	}
}

// extractedTableNames 保留给既有调用方：返回语句引用的表名（失败返回 nil）。
func extractedTableNames(sql string) []string {
	a, err := analyzeSQL(sql)
	if err != nil {
		return nil
	}
	return a.Tables
}

// canExecTableSQL 校验非管理员在 SQL 控制台访问的表是否在授权范围内。
func (u *User) canExecTableSQL(sql string) bool {
	return u.canExecTableSQLIn(sql, "")
}

// canExecTableSQLIn 同 canExecTableSQL，但按会话库限定未限定表名：
// USE mydb 之后，未限定名指向 mydb.<name>，必须按限定后的名字校验。
func (u *User) canExecTableSQLIn(sql, curDB string) bool {
	if u == nil {
		return false
	}
	if u.fullAccess() {
		return true
	}
	a, err := analyzeSQL(sql)
	if err != nil {
		return false // fail-closed：解析不出来就不放行
	}
	resolved := make([]string, 0, len(a.Tables))
	for _, tn := range a.Tables {
		lower := strings.ToLower(tn)
		if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") {
			continue // 系统元数据表对任意已登录用户可读
		}
		if !strings.Contains(tn, ".") && curDB != "" && curDB != "tsumugi" {
			tn = curDB + "." + tn
		}
		if !u.canUseTable(tn) {
			return false
		}
		resolved = append(resolved, tn)
	}
	// 列级读权限：与引擎侧 authorizeSQL 同一判定函数，避免两套语义。
	if a.Kind == sqlRead {
		if err := u.checkSelectColumns(resolved, a.Columns, a.Star); err != nil {
			return false
		}
	}
	return true
}

// ==================== 引擎侧（二进制 / MySQL 协议）授权 ====================

// authorizeSQL 校验某个已认证账号能否执行该语句（按表级 GRANT 判定）。
// 管理员（config.User）拥有全部权限；其余用户必须对每条被引用的表具备对应权限。
func (db *DB) authorizeSQL(user string, a sqlAccess) error {
	if user == "" {
		return fmt.Errorf("permission denied: not authenticated")
	}
	if user == db.rootUser() {
		return nil
	}
	var u *User
	if globalUsers != nil {
		u = globalUsers.Get(user)
	}
	// GRANT / REVOKE：权限变更只允许 admin 或 CanManage 执行。
	// 这里提前放行，避免要求执行者自己先持有 PermDDL；真正的授权范围与目标
	// 校验在 parseGrant 里完成（同一账号检查会再做一次，双保险）。
	if a.Kind == sqlGrant {
		if u != nil && (u.IsAdmin || u.CanManage) {
			return nil
		}
		return fmt.Errorf("permission denied: GRANT/REVOKE requires admin or manage privilege")
	}
	perm := a.Kind.perm()
	if a.Kind == sqlUse {
		// USE 会改写全局 curDB：只允许管理员通过协议执行（MySQL 连接级 USE 在协议层处理）
		return fmt.Errorf("permission denied: USE requires admin privilege")
	}
	if len(a.Tables) == 0 {
		// 无法定位表的写/DDL 语句：拒绝（`CREATE DATABASE` 等库级操作也在此列）
		// 读语句与会话级语句（BEGIN/COMMIT/SET/SAVEPOINT）不涉及表，任何已认证用户都可执行
		if a.Kind == sqlRead || a.Kind == sqlTxn {
			return nil
		}
		return fmt.Errorf("permission denied: %s requires table-level grant", a.Kind)
	}
	for _, t := range a.Tables {
		lower := strings.ToLower(t)
		if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") {
			continue // 只读元数据
		}
		// 系统表 __* 只对管理员开放，且没有对应的授权记录
		if strings.HasPrefix(t, "__") {
			return fmt.Errorf("permission denied: system table %s is admin-only", t)
		}
		// 只读：控制台用户按 dashboard 的库/表白名单放行（写/DDL 不走这条捷径，
		// 否则"可见即可写"会变成提权）
		if perm == PermSelect && u != nil && u.canUseTable(t) {
			continue
		}
		if !db.CheckPrivilege(user, t, perm) {
			return fmt.Errorf("permission denied: %s on table %s", a.Kind, t)
		}
	}
	// 列级 SELECT 白名单：在表级校验通过之后再判，错误信息能直接指出列名。
	// 走 acl.go 统一入口，Web 控制台与二进制 / MySQL 协议共用同一语义。
	if a.Kind == sqlRead && u != nil {
		if err := u.checkSelectColumns(a.Tables, a.Columns, a.Star); err != nil {
			return err
		}
	}
	return nil
}

// authorizeSQLText 便捷版本：分析并校验，返回分析结果供调用方复用。
// authorizeSQLTokensFast 热路径入口：管理员直接放行，省掉表名提取的切片分配；
// 非管理员才做完整分析（结果仅用于鉴权，调用方不消费 sqlAccess）。
func (db *DB) authorizeSQLTokensFast(user string, toks []sqlToken, schema string) error {
	if user == "" {
		return fmt.Errorf("permission denied: not authenticated")
	}
	if user == db.rootUser() {
		return nil
	}
	a, err := analyzeSQLTokens(toks)
	if err != nil {
		return err
	}
	// 带连接级默认库：`USE mydb` 之后的未限定表名必须按 mydb.<name> 校验，
	// 否则表级白名单与列级限制都可能被会话库绕过。
	return db.authorizeSQLIn(user, a, schema)
}

// authorizeSQLIn 同 authorizeSQL，但把未限定表名按会话库补齐，
// 避免 `USE mydb` 之后用未限定名访问 mydb 之外的表。
func (db *DB) authorizeSQLIn(user string, a sqlAccess, schema string) error {
	if schema != "" && schema != "tsumugi" && len(a.Tables) > 0 {
		tabs := make([]string, len(a.Tables))
		copy(tabs, a.Tables)
		for i, t := range tabs {
			if !strings.Contains(t, ".") {
				tabs[i] = schema + "." + t
			}
		}
		a.Tables = tabs
	}
	return db.authorizeSQL(user, a)
}

func (db *DB) authorizeSQLTokens(user string, toks []sqlToken) (sqlAccess, error) {
	a, err := analyzeSQLTokens(toks)
	if err != nil {
		return a, err
	}
	if err := db.authorizeSQL(user, a); err != nil {
		return a, err
	}
	return a, nil
}
