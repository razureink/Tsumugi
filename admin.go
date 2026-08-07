package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	if strings.Contains(name, ".") {
		return name
	}
	cur := db.getCurDB()
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
	}
	return "UNKNOWN"
}

func parseFieldType(s string) (FieldType, error) {
	switch strings.ToUpper(s) {
	case "INT", "INTEGER", "INT64", "BIGINT":
		return TypeInt, nil
	case "VARCHAR", "STRING", "TEXT", "CHAR":
		return TypeVarchar, nil
	case "BOOL", "BOOLEAN":
		return TypeBool, nil
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
	cols := make([]string, 0, len(t.meta.Fields))
	for _, f := range t.meta.Fields {
		cols = append(cols, f.Name)
	}
	var min *int64
	if afterPK >= 0 {
		v := afterPK
		min = &v
	}
	rows := make([][]interface{}, 0, limit)
	nextPK := int64(-1)
	t.pkTree.scanRangeLimit(min, limit, func(key int64, value []byte) bool {
		row, _, _ := decodeRow(t.meta, value)
		vals := make([]interface{}, 0, len(t.meta.Fields))
		for _, f := range t.meta.Fields {
			vals = append(vals, row[f.Name])
		}
		rows = append(rows, vals)
		nextPK = key
		return false
	})
	return map[string]interface{}{
		"columns":   cols,
		"rows":      rows,
		"next_pk":   nextPK,
		"row_count": t.pkTree.Size(),
	}, nil
}

// ==================== HTTP 处理器 ====================

func (db *DB) handleAdminTables(w http.ResponseWriter, r *http.Request) {
	dbName := r.URL.Query().Get("db")
	tables, err := db.adminTables(dbName)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]interface{}{
		"tables":    tables,
		"databases": mysqlCfg.get().Databases,
		"cur_db":    db.getCurDB(),
	})
}

func (db *DB) handleAdminRows(w http.ResponseWriter, r *http.Request) {
	table := r.URL.Query().Get("table")
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

func (db *DB) handleAdminQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	columns, rows, affected, rawMsg, err := db.runSQL(req.SQL)
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
	t := db.getTable(req.Table)
	if t == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "table not found"})
		return
	}
	pkVal, ok := req.Values[t.meta.PK]
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
	// 首次安装：显示向导；已安装：显示登录页
	if globalUsers.Count() == 0 {
		fmt.Fprint(w, setupWizardHTML)
	} else {
		fmt.Fprint(w, loginPageHTML)
	}
}

// handleSetupStatus 检查是否需要首次设置。
func (db *DB) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needSetup := globalUsers.Count() == 0
	writeJSON(w, map[string]interface{}{"ok": true, "need_setup": needSetup})
}

// handleSetupRootPwd 返回 root 初始密码（仅首次安装有效）。
func (db *DB) handleSetupRootPwd(w http.ResponseWriter, r *http.Request) {
	if globalUsers.Count() > 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "setup already completed"})
		return
	}
	rootPwd := generatePassword()
	os.WriteFile("data/.first_run_pwd", []byte(rootPwd), 0644)
	writeJSON(w, map[string]interface{}{"ok": true, "password": rootPwd})
}

// handleSetupComplete 完成首次设置：创建管理员账号。
func (db *DB) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if globalUsers.Count() > 0 {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "setup already completed"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
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
	// 同时保留 root 账号
	rootPwd := generatePassword()
	root := &User{
		Username:  "root",
		Password:  hashPasswd(rootPwd),
		IsAdmin:   true,
		CanStress: true,
		CanManage: true,
		CreatedAt: time.Now().Unix(),
	}
	globalUsers.Add(root)
	// 同步管理员凭据到配置（供 MySQL 协议认证使用）
	db.config.User = req.Username
	db.config.Password = req.Password
	persistConfig(db.config)
	// 清理临时文件
	os.Remove("data/.first_run_pwd")
	os.Remove("data/.first_run")
	logf(LOG_OK, "setup complete: admin=%s, root password regenerated", req.Username)
	tok := adminTokens.issue(req.Username)
	writeJSON(w, map[string]interface{}{"ok": true, "token": tok, "user": req.Username, "root_password": rootPwd})
}

// handleUserList 管理员获取用户列表。
func (db *DB) handleUserList(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "permission denied"})
		return
	}
	list := globalUsers.List()
	type safeUser struct {
		Username  string   `json:"username"`
		IsAdmin   bool     `json:"is_admin"`
		CanStress bool     `json:"can_stress"`
		CanManage bool     `json:"can_manage"`
		Databases []string `json:"databases"`
		CreatedAt int64    `json:"created_at"`
		LastLogin int64    `json:"last_login"`
	}
	safe := make([]safeUser, 0, len(list))
	for _, u := range list {
		safe = append(safe, safeUser{
			Username: u.Username, IsAdmin: u.IsAdmin, CanStress: u.CanStress,
			CanManage: u.CanManage, Databases: u.Databases, CreatedAt: u.CreatedAt, LastLogin: u.LastLogin,
		})
	}
	writeJSON(w, map[string]interface{}{"ok": true, "users": safe})
}

// handleUserCreate 管理员创建用户。
func (db *DB) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "permission denied"})
		return
	}
	var req struct {
		Username  string   `json:"username"`
		Password  string   `json:"password"`
		IsAdmin   bool     `json:"is_admin"`
		CanStress bool     `json:"can_stress"`
		CanManage bool     `json:"can_manage"`
		Databases []string `json:"databases"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid request"})
		return
	}
	if globalUsers.Get(req.Username) != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user already exists"})
		return
	}
	nu := &User{
		Username:  req.Username,
		Password:  hashPasswd(req.Password),
		IsAdmin:   req.IsAdmin,
		CanStress: req.CanStress,
		CanManage: req.CanManage,
		Databases: req.Databases,
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
		writeJSON(w, map[string]interface{}{"ok": false, "error": "permission denied"})
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
	globalUsers.Delete(req.Username)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleUserUpdate 管理员更新用户权限。
func (db *DB) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	user := requireUser(r)
	u := globalUsers.Get(user)
	if u == nil || !u.CanManage {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "permission denied"})
		return
	}
	var req struct {
		Username  string   `json:"username"`
		IsAdmin   *bool    `json:"is_admin,omitempty"`
		CanStress *bool    `json:"can_stress,omitempty"`
		CanManage *bool    `json:"can_manage,omitempty"`
		Databases []string `json:"databases,omitempty"`
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
	if req.IsAdmin != nil {
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
	if req.Password != "" {
		target.Password = hashPasswd(req.Password)
	}
	globalUsers.Update(target)
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
