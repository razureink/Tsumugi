package main

// ==================== C4：列级权限 + GRANT/REVOKE ====================

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newColumnPrivDB 建库 + 建表 t(id, pub, secret)。
func newColumnPrivDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, _, _, _, err := db.runSQL("CREATE TABLE t (id INT PRIMARY KEY, pub INT, secret INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db, dir
}

func addColumnPrivUser(t *testing.T, name string, canManage bool) {
	t.Helper()
	if globalUsers.Get(name) != nil {
		return
	}
	globalUsers.Add(&User{Username: name, Password: hashPasswd("x"), CanManage: canManage})
}

// 未配置列限制 = 不限制：原有用户行为完全不变（包括 SELECT *）。
func TestColumnPrivUnrestrictedByDefault(t *testing.T) {
	db, _ := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)

	if _, _, _, _, err := db.runSQLAs("bob", "SELECT * FROM t"); err != nil {
		t.Fatalf("unrestricted user should read all columns: %v", err)
	}
	u := globalUsers.Get("bob")
	if _, ok := u.columnRestriction("t"); ok {
		t.Fatal("column restriction must be absent by default")
	}
}

// 配置白名单后：允许列可查，被拒列报错（错误带列名，errno=1142）。
func TestColumnPrivGrantedAndDenied(t *testing.T) {
	db, _ := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)

	if _, _, _, _, err := db.runSQLAs("root", "GRANT SELECT (id, pub) ON t TO bob"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	u := globalUsers.Get("bob")
	allow, ok := u.columnRestriction("t")
	if !ok || len(allow) != 2 {
		t.Fatalf("restriction = %v ok=%v, want [id pub]", allow, ok)
	}
	if _, _, _, _, err := db.runSQLAs("bob", "SELECT id, pub FROM t"); err != nil {
		t.Fatalf("granted columns should be readable: %v", err)
	}

	for _, sql := range []string{
		"SELECT secret FROM t",
		"SELECT UPPER(secret) FROM t",
		"SELECT t.secret FROM t",
	} {
		_, _, _, _, err := db.runSQLAs("bob", sql)
		if err == nil {
			t.Fatalf("%q must be denied", sql)
		}
		if !strings.Contains(err.Error(), "secret") {
			t.Fatalf("%q: error must name the column, got %v", sql, err)
		}
		if got := mysqlErrno(err); got != ERColumnAccessDenied {
			t.Fatalf("%q: errno=%d want %d", sql, got, ERColumnAccessDenied)
		}
	}

	// SELECT * 不含显式列名：拒绝信息列出已授权列，并明确是通配符被拒。
	_, _, _, _, err := db.runSQLAs("bob", "SELECT * FROM t")
	if err == nil {
		t.Fatal("SELECT * must be denied when columns are restricted")
	}
	if !strings.Contains(err.Error(), "SELECT *") || !strings.Contains(err.Error(), "id") {
		t.Fatalf("star error must explain the granted columns, got %v", err)
	}
	if got := mysqlErrno(err); got != ERColumnAccessDenied {
		t.Fatalf("star errno=%d want %d", got, ERColumnAccessDenied)
	}

	// Web 控制台的布尔入口与引擎侧语义一致。
	bob := globalUsers.Get("bob")
	if bob.canExecTableSQL("SELECT secret FROM t") {
		t.Fatal("canExecTableSQL must reject denied column")
	}
	if !bob.canExecTableSQL("SELECT pub FROM t") {
		t.Fatal("canExecTableSQL must allow granted column")
	}
}

// GRANT SELECT ON t（无列列表）清除列限制；REVOKE 列取差集；
// 对未限制的表执行列级 REVOKE 会用表结构固化成"其余列"白名单。
func TestColumnPrivRevokeAndGrantAll(t *testing.T) {
	db, _ := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)
	addColumnPrivUser(t, "carol", false)

	if _, _, _, _, err := db.runSQLAs("root", "GRANT SELECT (id, pub) ON t TO bob"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("root", "REVOKE SELECT (pub) ON t FROM bob"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("bob", "SELECT id FROM t"); err != nil {
		t.Fatalf("id should survive revoke: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("bob", "SELECT pub FROM t"); err == nil {
		t.Fatal("pub must be denied after REVOKE")
	}

	// 无列列表的 GRANT = 恢复全列可读。
	if _, _, _, _, err := db.runSQLAs("root", "GRANT SELECT ON t TO bob"); err != nil {
		t.Fatalf("grant all columns: %v", err)
	}
	if _, ok := globalUsers.Get("bob").columnRestriction("t"); ok {
		t.Fatal("GRANT SELECT ON t must clear the column restriction")
	}
	if _, _, _, _, err := db.runSQLAs("bob", "SELECT * FROM t"); err != nil {
		t.Fatalf("all columns should be readable again: %v", err)
	}

	// carol 原本不受限：REVOKE secret 后只剩 id/pub。
	if _, _, _, _, err := db.runSQLAs("root", "REVOKE SELECT (secret) ON t FROM carol"); err != nil {
		t.Fatalf("revoke from unrestricted: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("carol", "SELECT secret FROM t"); err == nil {
		t.Fatal("secret must be denied for carol")
	}
	if _, _, _, _, err := db.runSQLAs("carol", "SELECT id, pub FROM t"); err != nil {
		t.Fatalf("other columns must stay readable: %v", err)
	}
}

// 列限制随 users.json 持久化：重开 UserStore 后仍在。
func TestColumnPrivPersistsInUsersJSON(t *testing.T) {
	db, dir := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)
	if _, _, _, _, err := db.runSQLAs("root", "GRANT SELECT (id) ON t TO bob"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// 直接读文件，确认字段确实写进 users.json（不新增文件）。
	raw, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	if !strings.Contains(string(raw), "column_read") || !strings.Contains(string(raw), "\"id\"") {
		t.Fatalf("users.json missing column_read: %s", raw)
	}

	// 模拟进程重启：重新加载同一目录的 UserStore。
	initUserStore(dir)
	if globalUsers == nil {
		t.Fatal("user store not initialized")
	}
	u := globalUsers.Get("bob")
	if u == nil {
		t.Fatal("bob lost after reload")
	}
	allow, ok := u.columnRestriction("t")
	if !ok || len(allow) != 1 || !strings.EqualFold(allow[0], "id") {
		t.Fatalf("restriction after reload = %v ok=%v", allow, ok)
	}
}

// 非管理员/非 CanManage 执行 GRANT/REVOKE 必须被拒；CanManage 允许。
func TestColumnPrivWhoCanGrant(t *testing.T) {
	db, _ := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)
	addColumnPrivUser(t, "carol", false)
	addColumnPrivUser(t, "dave", true) // 非管理员，但 CanManage

	if _, _, _, _, err := db.runSQLAs("bob", "GRANT SELECT (id) ON t TO carol"); err == nil {
		t.Fatal("plain user must not GRANT")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("bob", "REVOKE SELECT (id) ON t FROM carol"); err == nil {
		t.Fatal("plain user must not REVOKE")
	}

	if _, _, _, _, err := db.runSQLAs("dave", "GRANT SELECT (id) ON t TO carol"); err != nil {
		t.Fatalf("CanManage user should GRANT: %v", err)
	}
	if _, _, _, _, err := db.runSQLAs("dave", "REVOKE SELECT (id) ON t FROM carol"); err != nil {
		t.Fatalf("CanManage user should REVOKE: %v", err)
	}
}

// 只支持 SELECT；其它权限关键字、缺失用户/表/列都必须明确报错。
func TestColumnPrivExplicitErrors(t *testing.T) {
	db, _ := newColumnPrivDB(t)
	addColumnPrivUser(t, "bob", false)

	cases := []struct {
		sql  string
		want string
	}{
		{"GRANT INSERT ON t TO bob", "only SELECT"},
		{"GRANT ALL ON t TO bob", "only SELECT"},
		{"GRANT UPDATE (id) ON t TO bob", "only SELECT"},
		{"REVOKE DELETE ON t FROM bob", "only SELECT"},
		{"GRANT SELECT ON t TO ghost", "user not found"},
		{"GRANT SELECT ON no_such_table TO bob", "table not found"},
		{"GRANT SELECT (nope) ON t TO bob", "unknown column"},
		{"GRANT SELECT (id) ON t TO bob EXTRA", "unexpected"},
	}
	for _, c := range cases {
		_, _, _, _, err := db.runSQLAs("root", c.sql)
		if err == nil {
			t.Fatalf("%q must fail", c.sql)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
			t.Fatalf("%q: error %q must mention %q", c.sql, err, c.want)
		}
	}
}

// 列级拒绝映射到 1142，而非表级的 1044。
func TestColumnAccessErrno(t *testing.T) {
	err := &columnAccessError{Table: "t", Column: "secret"}
	if got := mysqlErrno(err); got != ERColumnAccessDenied {
		t.Fatalf("errno=%d want %d", got, ERColumnAccessDenied)
	}
	if got := sqlStateFor(ERColumnAccessDenied); got != "42000" {
		t.Fatalf("sqlstate=%s want 42000", got)
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error must name the column: %v", err)
	}
	star := &columnAccessError{Table: "t", Star: true}
	if got := mysqlErrno(star); got != ERColumnAccessDenied {
		t.Fatalf("star errno=%d want %d", got, ERColumnAccessDenied)
	}
}
