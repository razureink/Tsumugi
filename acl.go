package main

import (
	"net/http"
	"strings"
)

// ==================== 数据访问控制（ACL） ====================
// 非管理员用户可通过用户管理配置可用的数据库列表（Databases，空=全部）
// 与可用的表列表（Tables，空=下列数据库中的全部表），本文件在所有
// 数据管理接口处统一校验。管理员始终拥有全部权限。

// currentUser 从请求还原登录用户。
func currentUser(r *http.Request) *User {
	name := requireUser(r)
	if name == "" {
		return nil
	}
	return globalUsers.Get(name)
}

// fullAccess 判断用户是否为管理员（或未找到用户时默认放开，避免影响内部调用）。
func (u *User) fullAccess() bool {
	return u == nil || u.IsAdmin
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

// canUseDB 判断是否允许访问指定数据库。
func (u *User) canUseDB(dbname string) bool {
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
	if u.fullAccess() {
		return true
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
	if u == nil || u.fullAccess() {
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
	if u == nil || u.fullAccess() {
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

// canExecSQL 权限引擎：非管理员依据语句首关键字放行/拒绝。
// 返回 (允许, 拒绝原因)。
func (u *User) canExecSQL(upStmt string) (bool, string) {
	if u == nil || u.fullAccess() {
		return true, ""
	}
	kw := firstWord(upStmt)
	switch strings.ToUpper(kw) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "USE": // 只读/导航放行，表级在 canExecTableSQL 中校验
		return true, ""
	case "INSERT", "UPDATE", "DELETE", "ALTER", "CREATE", "DROP", "TRUNCATE", "RENAME", "SET":
		return false, "permission denied: write or DDL statements are admin-only"
	}
	return true, ""
}

// extractedTableNames 从语句中提取可能被访问的表名（FROM/JOIN/INTO/UPDATE/DELETE 后紧跟的标识符）。
func extractedTableNames(sql string) []string {
	var names []string
	toks, err := tokenizeSQL(sql)
	if err != nil {
		return nil
	}
	defer sqlTokenPool.Put(toks[:0])
	for i := 0; i+1 < len(toks); i++ {
		up := toks[i].upper
		if up != "FROM" && up != "JOIN" && up != "INTO" && up != "UPDATE" && up != "DELETE" {
			continue
		}
		j := i + 1
		if toks[j].kind == "kw" && (toks[j].upper == "TABLE" || toks[j].upper == "KEY") {
			j++
			if j >= len(toks) {
				continue
			}
		}
		if toks[j].kind != "ident" && toks[j].kind != "kw" {
			continue
		}
		name := toks[j].val
		if j+2 < len(toks) && toks[j+1].kind == "sym" && toks[j+1].val == "." && (toks[j+2].kind == "ident" || toks[j+2].kind == "kw") {
			name += "." + toks[j+2].val
		}
		names = append(names, name)
	}
	return names
}

// canExecTableSQL 校验非管理员在 SQL 控制台访问的表是否在授权范围内。
func (u *User) canExecTableSQL(sql string) bool {
	if u == nil || u.fullAccess() {
		return true
	}
	for _, tn := range extractedTableNames(sql) {
		lower := strings.ToLower(tn)
		if strings.HasPrefix(lower, "information_schema.") || strings.HasPrefix(lower, "mysql.") {
			continue // 系统元数据表对任意已登录用户可读
		}
		if !u.canUseTable(tn) {
			return false
		}
	}
	return true
}