package main

// ==================== 列级 SELECT 权限（C4） ====================
// 数据模型：User.ColumnRead（users.json 的 column_read 字段）
//
//	表名（小写，可带 "db." 前缀） → 允许读取的列（大小写不敏感）
//
// 语义是**白名单**：
//   - 未配置（map 为 nil、或表名不在 map 里）= 不限制，完全保持历史行为；
//   - 配置后只允许列出的列可读，`SELECT *` / `t.*` 一律拒绝；
//   - 键存在但值为空切片 = 该表任何列都不可读（REVOKE 掉全部列的结果）。
//
// GRANT / REVOKE 只支持 SELECT 的列级授予：
//
//	GRANT SELECT (col1, col2) ON tbl TO user
//	GRANT SELECT ON tbl TO user          -- 清除该表的列限制（恢复可读全部列）
//	REVOKE SELECT (col2) ON tbl FROM user
//	REVOKE SELECT ON tbl FROM user       -- 该表所有列都不可读（降级语义，见下）
//
// 其它权限关键字（INSERT/UPDATE/DELETE/ALL 等）明确报错，不假装成功。
// 执行者必须是 admin 或 CanManage；目标用户与目标表必须存在。
// 所有变更写入 users.json（UserStore.save），不新增文件。

import (
	"fmt"
	"strings"
)

// ERColumnAccessDenied 放在 mysql_errors.go 的常量区；这里只定义错误类型。
// columnAccessError 列级权限拒绝。Error() 必须带列名（客户端要能定位）。
type columnAccessError struct {
	Table   string
	Column  string // Star=true 时为空
	Star    bool
	Allowed []string // Star 时列出已授权列，便于定位
}

func (e *columnAccessError) Error() string {
	if e.Star {
		if len(e.Allowed) == 0 {
			return fmt.Sprintf("column access denied: SELECT * is not permitted on table %s (no column is granted)", e.Table)
		}
		return fmt.Sprintf("column access denied: SELECT * is not permitted on table %s (granted columns: %s)",
			e.Table, strings.Join(e.Allowed, ", "))
	}
	return fmt.Sprintf("column access denied: column '%s' in table '%s' is not granted", e.Column, e.Table)
}

// columnRestriction 返回某表的列白名单。ok=false 表示该表未配置限制。
// 表名匹配：先精确（小写），再退到不带库前缀的同一名字；配置里带库前缀而
// 查询名不带前缀时按基名匹配（同库部署下足够；多库同名表属极端场景）。
func (u *User) columnRestriction(table string) (allow []string, ok bool) {
	if u == nil || len(u.ColumnRead) == 0 {
		return nil, false
	}
	lt := strings.ToLower(table)
	if allow, ok := u.ColumnRead[lt]; ok {
		return allow, true
	}
	base := lt
	qualified := false
	if i := strings.LastIndexByte(lt, '.'); i >= 0 {
		base = lt[i+1:]
		qualified = true
	}
	if !qualified {
		if allow, ok := u.ColumnRead[base]; ok {
			return allow, true
		}
	} else if allow, ok := u.ColumnRead[base]; ok {
		// 查询名带库前缀、配置时没带：视为同一张表。
		return allow, true
	}
	if !qualified {
		for k, allow := range u.ColumnRead {
			if i := strings.LastIndexByte(k, '.'); i >= 0 && k[i+1:] == base {
				return allow, true
			}
		}
	}
	return nil, false
}

// checkSelectColumns 校验 SELECT 投影是否触碰被拒绝的列。
// tables 是语句引用的表（已按会话库补齐）；cols/star 来自 analyzeSQLTokens。
// 未配置列限制的用户完全不受影响（直接返回 nil）。
func (u *User) checkSelectColumns(tables []string, cols []sqlColumnRef, star bool) error {
	if u == nil || len(u.ColumnRead) == 0 {
		return nil
	}
	for _, tbl := range tables {
		lower := strings.ToLower(tbl)
		// 元数据表不受列限制（也不允许在上面配置列授权）。
		if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") ||
			strings.HasPrefix(tbl, "__") {
			continue
		}
		allow, restricted := u.columnRestriction(tbl)
		if !restricted {
			continue
		}
		if star {
			return &columnAccessError{Table: tbl, Star: true, Allowed: allow}
		}
		for _, ref := range cols {
			// 限定名指向另一张被引用的表时与本表无关。别名（如 `t AS x`）匹配
			// 不到任何表名，仍按本表校验——宁可误拒，不能漏放。
			if ref.Table != "" {
				if owner := referencedTableFor(tables, ref.Table); owner != "" && !strings.EqualFold(owner, tbl) {
					continue
				}
			}
			if !containsColumnFold(allow, ref.Column) {
				return &columnAccessError{Table: tbl, Column: ref.Column}
			}
		}
	}
	return nil
}

// referencedTableFor 把限定名（表名或 "db.表名" 的后半段）解析回 tables 里的表。
func referencedTableFor(tables []string, qual string) string {
	lq := strings.ToLower(qual)
	for _, t := range tables {
		lt := strings.ToLower(t)
		if lt == lq {
			return t
		}
		if i := strings.LastIndexByte(lt, '.'); i >= 0 && lt[i+1:] == lq {
			return t
		}
	}
	return ""
}

func containsColumnFold(list []string, name string) bool {
	for _, c := range list {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

// ==================== UserStore：列授权变更（持久化到 users.json） ====================

// cloneColumnMap 写时复制：Get 返回的 User 与 store 共享 map，直接修改会与
// 并发读产生数据竞争，也绕开 save。所有变更先克隆再整体替换指针。
func cloneColumnMap(src map[string][]string) map[string][]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

// grantSelectColumns 授予（并集）列读权限。
// cols 为空表示"不限制列"：删除该表的配置，恢复全列可读。
func (s *UserStore) grantSelectColumns(username, table string, cols []string) error {
	s.mu.Lock()
	u := s.users[username]
	if u == nil {
		s.mu.Unlock()
		return fmt.Errorf("user not found: %s", username)
	}
	m := cloneColumnMap(u.ColumnRead)
	if m == nil {
		m = make(map[string][]string, 1)
	}
	if len(cols) == 0 {
		delete(m, table)
	} else if cur, ok := m[table]; ok {
		merged := append([]string(nil), cur...)
		for _, c := range cols {
			if !containsColumnFold(merged, c) {
				merged = append(merged, c)
			}
		}
		m[table] = merged
	} else {
		m[table] = append([]string(nil), cols...)
	}
	u.ColumnRead = m
	s.mu.Unlock()
	s.save()
	return nil
}

// revokeSelectColumns 撤销列读权限（差集）。
// cols 为空：该表所有列都不可读（显式写入空白名单）。
// cols 非空：若该表原本未配置限制，则用表结构把"除被撤销列之外"固化成白名单。
func (s *UserStore) revokeSelectColumns(username, table string, cols []string, allColumns []string) error {
	s.mu.Lock()
	u := s.users[username]
	if u == nil {
		s.mu.Unlock()
		return fmt.Errorf("user not found: %s", username)
	}
	m := cloneColumnMap(u.ColumnRead)
	if m == nil {
		m = make(map[string][]string, 1)
	}
	if len(cols) == 0 {
		m[table] = []string{}
	} else if cur, ok := m[table]; ok {
		m[table] = subtractColumns(cur, cols)
	} else {
		m[table] = subtractColumns(allColumns, cols)
	}
	u.ColumnRead = m
	s.mu.Unlock()
	s.save()
	return nil
}

func subtractColumns(list, remove []string) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		if !containsColumnFold(remove, c) {
			out = append(out, c)
		}
	}
	return out
}

// ==================== 解析 GRANT / REVOKE ====================

// canManageColumnPrivileges 判断执行者是否有权变更列授权：admin 或 CanManage。
// user==""（内部可信调用）不算执行者，直接拒绝——权限变更必须有人负责。
func (db *DB) canManageColumnPrivileges(user string) bool {
	if user == "" {
		return false
	}
	if user == db.rootUser() {
		return true
	}
	if globalUsers == nil {
		return false
	}
	u := globalUsers.Get(user)
	return u != nil && (u.IsAdmin || u.CanManage)
}

func tableColumnNames(t *Table) []string {
	meta := t.Meta()
	if meta == nil {
		return nil
	}
	out := make([]string, 0, len(meta.Fields))
	for _, f := range meta.Fields {
		out = append(out, f.Name)
	}
	return out
}

// canonicalColumn 按表结构做大小写不敏感匹配，返回元数据里的规范列名。
func canonicalColumn(t *Table, name string) (string, bool) {
	meta := t.Meta()
	if meta == nil {
		return "", false
	}
	for _, f := range meta.Fields {
		if strings.EqualFold(f.Name, name) {
			return f.Name, true
		}
	}
	return "", false
}

// parseGrant 处理文本层 GRANT / REVOKE（revoke=true 表示 REVOKE）。
func (db *DB) parseGrant(p *sqlParser, revoke bool) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // GRANT / REVOKE
	verb := "GRANT"
	if revoke {
		verb = "REVOKE"
	}
	if !db.canManageColumnPrivileges(p.user) {
		err = fmt.Errorf("permission denied: %s requires admin or manage privilege", verb)
		return
	}
	// 只支持 SELECT；其它权限关键字明确拒绝（不能假成功）。
	if !p.peek().is("SELECT") {
		err = fmt.Errorf("%s supports only SELECT privilege; %q is not supported", verb, p.peek().val)
		return
	}
	p.next() // SELECT

	var cols []string
	if p.matchSym("(") {
		for {
			c, e := p.readTableIdent()
			if e != nil {
				err = fmt.Errorf("expected column name in %s: %v", verb, e)
				return
			}
			cols = append(cols, c)
			if p.matchSym(",") {
				continue
			}
			if !p.matchSym(")") {
				err = fmt.Errorf("expected ) after column list in %s", verb)
				return
			}
			break
		}
	}
	if p.peek().kind == kindSym && p.peek().val == "," {
		err = fmt.Errorf("%s supports only a single SELECT privilege (column-level grants)", verb)
		return
	}
	if !p.matchKeyword("ON") {
		err = fmt.Errorf("expected ON in %s", verb)
		return
	}
	tbl, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tbl = db.qualifyTableIn(tbl, p.curSchema(db))
	if revoke {
		if !p.matchKeyword("FROM") {
			err = fmt.Errorf("expected FROM in REVOKE")
			return
		}
	} else {
		if !p.matchKeyword("TO") {
			err = fmt.Errorf("expected TO in GRANT")
			return
		}
	}
	target, e := p.readTableIdent()
	if e != nil {
		err = fmt.Errorf("expected user name in %s", verb)
		return
	}
	// MySQL 风格 'user'@'host'：按用户名授权，host 部分忽略（单机部署）。
	if p.peek().kind == kindSym && p.peek().val == "@" {
		p.next()
		if _, he := p.readTableIdent(); he != nil {
			err = he
			return
		}
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}

	t := db.getTable(tbl)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tbl)
		return
	}
	if globalUsers == nil || globalUsers.Get(target) == nil {
		err = fmt.Errorf("user not found: %s", target)
		return
	}
	// 列必须真实存在：授权一个不存在的列是无意义配置，直接报错。
	if len(cols) > 0 {
		canon := make([]string, 0, len(cols))
		for _, c := range cols {
			name, ok := canonicalColumn(t, c)
			if !ok {
				err = fmt.Errorf("unknown column '%s' in table %s", c, tbl)
				return
			}
			canon = append(canon, name)
		}
		cols = canon
	}
	if revoke {
		err = globalUsers.revokeSelectColumns(target, tbl, cols, tableColumnNames(t))
	} else {
		err = globalUsers.grantSelectColumns(target, tbl, cols)
	}
	if err != nil {
		return
	}
	rawMsg = "Query OK"
	return
}
