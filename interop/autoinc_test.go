package interop

import (
	"testing"
)

// 自增主键：INSERT 不带主键列时由引擎分配，并通过 MySQL OK 包的
// last insert id 返回（go-sql-driver 的 res.LastInsertId()）。
// 这正是 ORM（GORM 等）取自增主键的路径。
func TestAutoIncrementLastInsertID(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE ai_t (id INT PRIMARY KEY, name VARCHAR(20))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := db.Exec("INSERT INTO ai_t (name) VALUES ('a')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if id <= 0 {
		t.Fatalf("last insert id = %d, want > 0", id)
	}
	var got int64
	if err := db.QueryRow("SELECT id FROM ai_t WHERE name = 'a'").Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != id {
		t.Fatalf("row id = %d, last insert id = %d", got, id)
	}
	res2, err := db.Exec("INSERT INTO ai_t (name) VALUES ('b')")
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	id2, err := res2.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id 2: %v", err)
	}
	if id2 <= id {
		t.Fatalf("second auto id = %d, want > %d", id2, id)
	}
	// 显式主键不影响自增计数器的单调性
	if _, err := db.Exec("INSERT INTO ai_t (id, name) VALUES (500, 'c')"); err != nil {
		t.Fatalf("insert explicit: %v", err)
	}
	res3, err := db.Exec("INSERT INTO ai_t (name) VALUES ('d')")
	if err != nil {
		t.Fatalf("insert 3: %v", err)
	}
	id3, _ := res3.LastInsertId()
	if id3 <= 500 {
		t.Fatalf("auto id after explicit 500 = %d, want > 500", id3)
	}
}
