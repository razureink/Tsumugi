package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ==================== 管理面板后端 API ====================

func (db *DB) getTable(name string) *Table {
	v, ok := db.tables.Load(name)
	if !ok {
		return nil
	}
	return v.(*Table)
}

// lookupTable 带锁读取表（兼容旧调用约定），并做类型断言。
func (db *DB) lookupTable(name string) (*Table, bool) {
	db.mu.RLock()
	v, ok := db.tables.Load(name)
	db.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return v.(*Table), true
}

// getCurDB 返回当前会话数据库（默认 tsumugi）。
func (db *DB) getCurDB() string {
	db.curDBMu.RLock()
	defer db.curDBMu.RUnlock()
	if db.curDB == "" {
		return "tsumugi"
	}
	return db.curDB
}

// setCurDB 设置当前会话数据库。
func (db *DB) setCurDB(name string) {
	db.curDBMu.Lock()
	db.curDB = name
	db.curDBMu.Unlock()
}

// qualifyTable 将表名解析为物理表名：
// 已含 "." 前缀的原样返回；否则按当前数据库加前缀（默认库 tsumugi 不加前缀，保持兼容）。
func (db *DB) qualifyTable(name string) string {
	return db.qualifyTableIn(name, "")
}

// qualifyTableIn 同 qualifyTable，但使用显式指定的库名（空值回退到全局 curDB）。
func (db *DB) qualifyTableIn(name, schema string) string {
	if strings.Contains(name, ".") {
		return name
	}
	cur := schema
	if cur == "" {
		cur = db.getCurDB()
	}
	if cur == "" || cur == "tsumugi" {
		return name
	}
	return cur + "." + name
}

// adminTables 返回所有表及元信息、行数。
// dbName 非空时只返回该库下的表（前缀匹配；tsumugi 库返回无前缀表）。
func (db *DB) adminTables(dbName string) ([]map[string]interface{}, error) {
	db.mu.RLock()
	tables := make([]map[string]interface{}, 0, len(db.catalog))
	for name, meta := range db.catalog {
		if dbName != "" {
			if dbName == "tsumugi" {
				if strings.Contains(name, ".") {
					continue
				}
			} else if !strings.HasPrefix(name, dbName+".") {
				continue
			}
		}
		t := db.getTable(name)
		var rowCount int64
		if t != nil {
			rowCount = t.pkTree.Size()
		}
		fields := make([]map[string]interface{}, 0, len(meta.Fields))
		for _, f := range meta.Fields {
			fields = append(fields, map[string]interface{}{
				"name": f.Name,
				"type": fieldTypeName(f.Type),
				"len":  f.Len,
			})
		}
		indexes := make([]map[string]interface{}, 0, len(meta.Indexes))
		for idxName, fieldName := range meta.Indexes {
			indexes = append(indexes, map[string]interface{}{
				"name":  idxName,
				"field": fieldName,
			})
		}
		tables = append(tables, map[string]interface{}{
			"name":      name,
			"pk":        meta.PK,
			"fields":    fields,
			"indexes":   indexes,
			"row_count": rowCount,
		})
	}
	db.mu.RUnlock()
	return tables, nil
}

func fieldTypeName(t FieldType) string {
	switch t {
	case TypeInt:
		return "INT"
	case TypeVarchar:
		return "VARCHAR"
	case TypeBool:
		return "BOOL"
	case TypeDate:
		return "DATE"
	case TypeDatetime:
		return "DATETIME"
	case TypeDecimal:
		return "DECIMAL"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	}
	return "UNKNOWN"
}

// fieldTypeDecl 返回带参数的 DDL 类型文本（SHOW CREATE / DESCRIBE / COLUMN_TYPE）。
// 只有 DECIMAL 需要参数：精度 p 在 DDL 解析时校验但不落盘（存储上限由 int64
// 决定），这里按支持的最大精度 18 回显，确保 DECIMAL(10,2) 的 scale 能往返。
func fieldTypeDecl(f Field) string {
	if f.Type == TypeDecimal {
		return fmt.Sprintf("DECIMAL(18,%d)", f.Len)
	}
	return fieldTypeName(f.Type)
}

func parseFieldType(s string) (FieldType, error) {
	switch strings.ToUpper(s) {
	case "INT", "INTEGER", "INT64", "BIGINT", "SMALLINT", "TINYINT", "MEDIUMINT":
		return TypeInt, nil
	case "VARCHAR", "STRING", "CHAR":
		return TypeVarchar, nil
	case "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT":
		return TypeText, nil
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "VARBINARY", "BINARY":
		return TypeBlob, nil
	case "BOOL", "BOOLEAN":
		return TypeBool, nil
	case "DATE":
		return TypeDate, nil
	case "DATETIME", "TIMESTAMP":
		return TypeDatetime, nil
	case "DECIMAL", "NUMERIC", "DEC", "FIXED":
		return TypeDecimal, nil
	}
	return 0, fmt.Errorf("unsupported type: %s", s)
}

// adminRows 基于主键 keyset 分页取表数据
func (db *DB) adminRows(tableName string, afterPK int64, limit int) (map[string]interface{}, error) {
	t := db.getTable(tableName)
	if t == nil {
		return nil, fmt.Errorf("table not found: %s", tableName)
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	cols := make([]string, 0, len(t.Meta().Fields))
	for _, f := range t.Meta().Fields {
		cols = append(cols, f.Name)
	}
	// keyset 分页：afterPK 是上一页最后一行的主键，必须从 afterPK+1 开始，
	// 否则每页首行与上一页末行重复（scanRangeLimit 的下界是闭区间）。
	var min *int64
	if afterPK >= 0 && afterPK < math.MaxInt64 {
		v := afterPK + 1
		min = &v
	}
	rows := make([][]interface{}, 0, limit)
	pks := make([]int64, 0, limit)
	nextPK := int64(-1)
	t.pkTree.scanRangeLimit(min, limit, func(key int64, value []byte) bool {
		row, _, _ := decodeRow(t.Meta(), value)
		vals := make([]interface{}, 0, len(t.Meta().Fields))
		for _, f := range t.Meta().Fields {
			vals = append(vals, row[f.Name])
		}
		rows = append(rows, vals)
		pks = append(pks, key)
		nextPK = key
		return false
	})
	return map[string]interface{}{
		"columns":   cols,
		"rows":      rows,
		"pks":       pks, // 与 rows 同序：前端删除必须用真实主键，而不是首列
		"next_pk":   nextPK,
		"row_count": t.pkTree.Size(),
	}, nil
}

// ==================== HTTP 处理器 ====================

func (db *DB) handleAdminTables(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	dbName := r.URL.Query().Get("db")
	if dbName != "" && !u.canUseDB(dbName) {
		denyJSON(w, "database not allowed")
		return
	}
	tables, err := db.adminTables(dbName)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	cur := db.getCurDB()
	if dbName != "" && mysqlCfg.hasDatabase(dbName) {
		cur = dbName
	}
	writeJSON(w, map[string]interface{}{
		"tables":    filterTables(tables, u),
		"databases": filterDBList(mysqlCfg.get().Databases, u),
		"cur_db":    cur,
	})
}

// validDBName 校验数据库名（标识符规则，避开注入风险）。
func validDBName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, r := range name {
		if i == 0 && !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
		if i > 0 && !(r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// denyJSON 写统一格式的拒绝响应（permission denied 前缀）。
func denyJSON(w http.ResponseWriter, reason string) {
	msg := "permission denied"
	if reason != "" {
		msg += ": " + reason
	}
	writeJSON(w, map[string]interface{}{"ok": false, "error": msg})
}

// requireAdmin 校验管理员权限；不满足时写拒绝响应并返回 false。
func requireAdmin(w http.ResponseWriter, r *http.Request, reason string) bool {
	u := currentUser(r)
	if u != nil && u.fullAccess() {
		return true
	}
	denyJSON(w, reason)
	return false
}

// adminDBName 解码数据库管理请求，返回去除首尾空白的库名。
func adminDBName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req dbManageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return "", false
	}
	return strings.TrimSpace(req.Name), true
}

// handleAdminDBCreate 新建数据库：写入 mysqlCfg.Databases（持久化），
// 与 SQL 控制台 CREATE DATABASE 走同一引擎路径，保证各处列表同步。
func (db *DB) handleAdminDBCreate(w http.ResponseWriter, r *http.Request) {
	name, ok := adminDBName(w, r)
	if !ok {
		return
	}
	if !validDBName(name) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid database name"})
		return
	}
	if !requireAdmin(w, r, "creating databases is admin-only") {
		return
	}
	_, _, _, rawMsg, err := db.runSQL("CREATE DATABASE " + name)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":        true,
		"message":   rawMsg,
		"databases": mysqlCfg.get().Databases,
	})
}

// handleAdminDBDrop 删除数据库（连同库下所有表）。系统内置库受保护。
func (db *DB) handleAdminDBDrop(w http.ResponseWriter, r *http.Request) {
	name, ok := adminDBName(w, r)
	if !ok {
		return
	}
	for _, sys := range []string{"information_schema", "mysql", "performance_schema", "sys"} {
		if strings.EqualFold(name, sys) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "system database cannot be dropped"})
			return
		}
	}
	if !validDBName(name) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid database name"})
		return
	}
	if !requireAdmin(w, r, "dropping databases is admin-only") {
		return
	}
	_, _, _, _, err := db.runSQL("DROP DATABASE " + name)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":        true,
		"message":   "database dropped",
		"databases": mysqlCfg.get().Databases,
		"cur_db":    db.getCurDB(),
	})
}

type dbManageRequest struct {
	Name string `json:"name"`
}

func (db *DB) handleAdminRows(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	table := r.URL.Query().Get("table")
	if !u.canUseTable(table) {
		denyJSON(w, "table not allowed")
		return
	}
	afterPK := int64(-1)
	if v := r.URL.Query().Get("after_pk"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			afterPK = n
		}
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	res, err := db.adminRows(table, afterPK, limit)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, res)
}

type queryRequest struct {
	SQL string `json:"sql"`
}

// truncateForLog 截断过长输入，避免审计日志被单条 SQL 撑爆。
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// webSchemaFor 取某令牌的 SQL 控制台会话库（USE 的作用域）。
// 会话库按 token 隔离，不再写入全局 curDB——否则一个受限用户 USE 后，
// 其他用户（含管理员）的未限定表名解析都会被带偏，且 canExecTableSQL
// 按"未限定名属于默认库"校验可被 USE 绕过（回归 H3）。
func (db *DB) webSchemaFor(token string) string {
	if v, ok := db.webSchemas.Load(token); ok {
		return v.(string)
	}
	return ""
}

// handleWebUse 处理 SQL 控制台的 USE：校验目标库后记到该 token 的会话上。
func (db *DB) handleWebUse(u *User, token, sql string) (bool, string) {
	toks, err := tokenizeSQL(sql)
	if err != nil || len(toks) < 2 || (toks[1].kind != kindIdent && toks[1].kind != kindStr) {
		return false, "invalid USE statement"
	}
	name := toks[1].val
	putSQLTokens(toks)
	if !mysqlCfg.hasDatabase(name) {
		return false, "unknown database " + name
	}
	if !u.canUseDB(name) {
		return false, "permission denied: database " + name
	}
	if name == "tsumugi" {
		db.webSchemas.Delete(token)
	} else {
		db.webSchemas.Store(token, name)
	}
	return true, ""
}

func (db *DB) handleAdminQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u := currentUser(r)
	tok := bearerToken(r)
	schema := db.webSchemaFor(tok)
	// 一次分词得到语句类型 + 被引用的表：判定不再依赖首词猜测
	access, aerr := analyzeSQL(req.SQL)
	if aerr != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": aerr.Error()})
		return
	}
	// USE 在会话层处理：校验后仅对本 token 生效
	if access.Kind == sqlUse {
		ok, msg := db.handleWebUse(u, tok, req.SQL)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": msg})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "message": "Database changed"})
		return
	}
	user := ""
	if u != nil {
		user = u.Username
	}
	if err := db.authorizeSQLIn(user, access, schema); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	// 审计：写/DDL 语句留痕（读查询量大，不记）
	if access.Kind != sqlRead && access.Kind != sqlUse && u != nil {
		logf(LOG_VERB, "audit: user=%s action=sql kind=%d sql=%q", u.Username, access.Kind, truncateForLog(req.SQL, 200))
	}
	// 带执行账号运行：GRANT/REVOKE 需要在解析层知道"谁在执行"（admin/CanManage），
	// 同时再走一次引擎侧授权，防止 Web 预检与执行之间的语义漂移。
	columns, rows, affected, rawMsg, err := db.runSQLSchemaAs(user, req.SQL, nil, 0, schema)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"columns":  columns,
		"rows":     rows,
		"affected": affected,
		"message":  rawMsg,
	})
}

type insertRequest struct {
	Table  string                 `json:"table"`
	Values map[string]interface{} `json:"values"`
}

func (db *DB) handleAdminInsert(w http.ResponseWriter, r *http.Request) {
	var req insertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u := currentUser(r)
	if !u.canUseTable(req.Table) {
		denyJSON(w, "table not allowed")
		return
	}
	t := db.getTable(req.Table)
	if t == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "table not found"})
		return
	}
	pkVal, ok := req.Values[t.Meta().PK]
	if !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "missing primary key"})
		return
	}
	pk, ok := pkVal.(float64)
	if !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "primary key must be integer"})
		return
	}
	if err := t.Insert(int64(pk), req.Values, 0, 0, false); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "message": "1 row inserted"})
}

type deleteRequest struct {
	Table string `json:"table"`
	PK    int64  `json:"pk"`
}

func (db *DB) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u := currentUser(r)
	if !u.canUseTable(req.Table) {
		denyJSON(w, "table not allowed")
		return
	}
	t := db.getTable(req.Table)
	if t == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "table not found"})
		return
	}
	if err := t.Delete(req.PK, 0, false); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "message": "1 row deleted"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (db *DB) handleRootPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 同上：登录/向导页的样式也是内嵌的，禁止缓存避免升级后仍是旧外观
	w.Header().Set("Cache-Control", "no-store")
	// 首次安装：显示向导；已安装：显示登录页
	if globalUsers.Count() == 0 {
		fmt.Fprint(w, injectPanelAssets(strings.Replace(setupWizardHTML, "/*__I18N__*/", cachedWizDictJS(), 1)))
	} else {
		fmt.Fprint(w, injectPanelAssets(strings.Replace(loginPageHTML, "/*__I18N__*/", cachedDashDictJS(), 1)))
	}
}

// handleSetupStatus 检查是否需要首次设置。
func (db *DB) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needSetup := globalUsers.Count() == 0 && !userStoreCorrupt
	writeJSON(w, map[string]interface{}{"ok": true, "need_setup": needSetup})
}

// setupDone 安装向导单次完成标记（原子 CAS，防并发重复建管理员）。
var setupDone atomic.Bool

// validUsername 校验用户名字符集：字母/数字/下划线/短横线，1~64 字节。
// 用户名会被渲染进管理页 JS 上下文，收紧字符集从源头消除存储型 XSS。
func validUsername(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// handleSetupComplete 完成首次设置：创建管理员账号。
func (db *DB) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if globalUsers.Count() > 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "setup already completed"})
		return
	}
	if userStoreCorrupt {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user store corrupt; manual intervention required"})
		return
	}
	// 单次安装：并发请求只有一个能通过 CAS，防止同时创建两个管理员
	if !setupDone.CompareAndSwap(false, true) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "setup already completed"})
		return
	}
	defer setupDone.Store(false)
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" || !validUsername(req.Username) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid request"})
		return
	}
	// 创建管理员
	admin := &User{
		Username:  req.Username,
		Password:  hashPasswd(req.Password),
		IsAdmin:   true,
		CanStress: true,
		CanManage: true,
		CreatedAt: time.Now().Unix(),
	}
	globalUsers.Add(admin)
	// 同步管理员凭据到配置（供 MySQL 协议认证使用）：仅持久化 SHA1 摘要，不落明文
	// cfgMu 与设置页的并发写入互斥（回归 M2）
	db.cfgMu.Lock()
	db.config.User = req.Username
	db.config.PasswordSHA1 = passwordSHA1Hex(req.Password)
	db.config.Password = "" // 明文只在本函数栈上存在，不驻留、不落盘
	persistConfig(db.config)
	db.cfgMu.Unlock()
	logf(LOG_OK, "setup complete: admin=%s (MySQL root 账号使用同一口令)", req.Username)
	tok := adminTokens.issue(req.Username)
	// 不再把口令回传给浏览器：响应体里的凭据会被代理日志/浏览器历史留存
	writeJSON(w, map[string]interface{}{"ok": true, "token": tok, "user": req.Username})
}

// handleUserList 管理员获取用户列表。
func (db *DB) handleUserList(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		denyJSON(w, "")
		return
	}
	list := globalUsers.List()
	type safeUser struct {
		Username   string              `json:"username"`
		IsAdmin    bool                `json:"is_admin"`
		CanStress  bool                `json:"can_stress"`
		CanManage  bool                `json:"can_manage"`
		Databases  []string            `json:"databases"`
		Tables     []string            `json:"tables"`
		ColumnRead map[string][]string `json:"column_read,omitempty"`
		CreatedAt  int64               `json:"created_at"`
		LastLogin  int64               `json:"last_login"`
	}
	safe := make([]safeUser, 0, len(list))
	for _, u := range list {
		safe = append(safe, safeUser{
			Username: u.Username, IsAdmin: u.IsAdmin, CanStress: u.CanStress,
			CanManage: u.CanManage, Databases: u.Databases, Tables: u.Tables,
			ColumnRead: u.ColumnRead,
			CreatedAt:  u.CreatedAt, LastLogin: u.LastLogin,
		})
	}
	writeJSON(w, map[string]interface{}{"ok": true, "users": safe})
}

// handleUserCreate 管理员创建用户。
func (db *DB) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		denyJSON(w, "")
		return
	}
	var req struct {
		Username  string   `json:"username"`
		Password  string   `json:"password"`
		IsAdmin   bool     `json:"is_admin"`
		CanStress bool     `json:"can_stress"`
		CanManage bool     `json:"can_manage"`
		Databases []string `json:"databases"`
		Tables    []string `json:"tables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid request"})
		return
	}
	if !validUsername(req.Username) {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid username (allowed: letters, digits, _, -, max 64)"})
		return
	}
	if globalUsers.Get(req.Username) != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user already exists"})
		return
	}
	// 提权防护：只有管理员能创建管理员
	if req.IsAdmin && !u.IsAdmin {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "only admins can create admins"})
		return
	}
	nu := &User{
		Username:  req.Username,
		Password:  hashPasswd(req.Password),
		IsAdmin:   u.IsAdmin && req.IsAdmin,
		CanStress: req.CanStress,
		CanManage: req.CanManage,
		Databases: req.Databases,
		Tables:    req.Tables,
		CreatedAt: time.Now().Unix(),
	}
	globalUsers.Add(nu)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleUserDelete 管理员删除用户。
func (db *DB) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		denyJSON(w, "")
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid request"})
		return
	}
	if req.Username == user {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "cannot delete yourself"})
		return
	}
	target := globalUsers.Get(req.Username)
	if target == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user not found"})
		return
	}
	// 提权防护：CanManage 而非管理员不能删除管理员
	if target.IsAdmin && !u.IsAdmin {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "only admins can delete admins"})
		return
	}
	globalUsers.Delete(req.Username)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleUserUpdate 管理员更新用户权限。
func (db *DB) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		denyJSON(w, "")
		return
	}
	var req struct {
		Username  string   `json:"username"`
		IsAdmin   *bool    `json:"is_admin,omitempty"`
		CanStress *bool    `json:"can_stress,omitempty"`
		CanManage *bool    `json:"can_manage,omitempty"`
		Databases []string `json:"databases,omitempty"`
		Tables    []string `json:"tables,omitempty"`
		Password  string   `json:"password,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid request"})
		return
	}
	target := globalUsers.Get(req.Username)
	if target == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user not found"})
		return
	}
	// 提权防护：CanManage 而非管理员不能修改管理员账号的任何属性
	// （否则可重置管理员密码完成提权）。
	if target.IsAdmin && !u.IsAdmin {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "only admins can modify admins"})
		return
	}
	pwdChanged := false
	if req.IsAdmin != nil && u.IsAdmin {
		target.IsAdmin = *req.IsAdmin
	}
	if req.CanStress != nil {
		target.CanStress = *req.CanStress
	}
	if req.CanManage != nil {
		target.CanManage = *req.CanManage
	}
	if req.Databases != nil {
		target.Databases = req.Databases
	}
	if req.Tables != nil {
		target.Tables = req.Tables
	}
	if req.Password != "" {
		target.Password = hashPasswd(req.Password)
		pwdChanged = true
	}
	globalUsers.Update(target)
	// 改密后吊销该用户全部活动会话，旧令牌立即失效
	if pwdChanged {
		adminTokens.revokeAll(target.Username)
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleCurrentUser 获取当前登录用户信息。
func (db *DB) handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	if user == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "not logged in"})
		return
	}
	u := globalUsers.Get(user)
	if u == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user not found"})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "username": u.Username, "is_admin": u.IsAdmin,
		"can_stress": u.CanStress, "can_manage": u.CanManage, "databases": u.Databases,
	})
}

func (db *DB) handleAppPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := "dashboard"
	if len(r.URL.Path) > 1 && strings.HasPrefix(r.URL.Path, "/admin") {
		page = "admin"
	} else if len(r.URL.Path) > 1 && strings.HasPrefix(r.URL.Path, "/users") {
		page = "users"
	}
	renderApp(w, db, page)
}
