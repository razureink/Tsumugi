package main

// ==================== C1：物理 WAL 传送复制 ====================
// 单进程只能有一个 DB 实例（包级 WAL 单例），所以测试用**顺序实例**模拟：
//   DB1 写入并关闭 → 用它的 WAL 前缀给 DB2 做初始状态 → 把 DB1 的增量
//   交给 DB2 的 ApplyWALDelta → 验证行数/LSN/CRC 拒绝/只读/提升。
// 这正好覆盖真实复制里最容易错的三个点：帧边界、LSN 对齐、只读语义。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func readFileRange(t *testing.T, path string, from, to int64) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	buf := make([]byte, to-from)
	if _, err := f.ReadAt(buf, from); err != nil {
		t.Fatalf("read %s [%d,%d): %v", path, from, to, err)
	}
	return buf
}

func countRows(t *testing.T, db *DB, table string) int64 {
	t.Helper()
	rows := rowsOf(t, db, "SELECT COUNT(*) FROM "+table)
	if len(rows) != 1 {
		t.Fatalf("count returned %d rows", len(rows))
	}
	n, _ := rows[0][0].(int64)
	return n
}

// TestReplicationBootstrapAndApply 初始全量 + 增量应用，LSN 严格对齐。
func TestReplicationBootstrapAndApply(t *testing.T) {
	SetLogMute(true)
	dirA := t.TempDir()
	cfgA := walTestConfig(dirA)
	cfgA.WALCompress = "on"
	db1, err := NewDB(cfgA)
	if err != nil {
		t.Fatalf("db1: %v", err)
	}
	mustRun(t, db1, "CREATE TABLE ship_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db1, "INSERT INTO ship_t VALUES (1,10)")
	mustRun(t, db1, "INSERT INTO ship_t VALUES (2,20)")
	pos1, err := db1.WALPosition()
	if err != nil {
		t.Fatalf("WALPosition: %v", err)
	}
	mustRun(t, db1, "INSERT INTO ship_t VALUES (3,30)")
	db1.Close()

	walA := filepath.Join(dirA, "wal")
	st, err := os.Stat(walA)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	end := st.Size()
	delta := readFileRange(t, walA, pos1, end)
	if len(delta) == 0 {
		t.Fatal("expected a non-empty delta")
	}

	// 用主库 WAL 的前缀字节做从库初始状态（等价于 Backup/RestorePointInTime）
	dirB := t.TempDir()
	if err := copyWALPrefix(walA, filepath.Join(dirB, "wal"), pos1); err != nil {
		t.Fatalf("bootstrap copy: %v", err)
	}
	cfgB := walTestConfig(dirB)
	cfgB.WALCompress = "on"
	db2, err := NewDB(cfgB)
	if err != nil {
		t.Fatalf("db2: %v", err)
	}
	defer db2.Close()
	if n := countRows(t, db2, "ship_t"); n != 2 {
		t.Fatalf("bootstrap rows=%d want 2", n)
	}
	if got := db2.replicaOffset.Load(); got != pos1 {
		t.Fatalf("bootstrap offset=%d want %d", got, pos1)
	}

	if err := db2.ApplyWALDelta(delta, true, true); err != nil {
		t.Fatalf("ApplyWALDelta: %v", err)
	}
	if n := countRows(t, db2, "ship_t"); n != 3 {
		t.Fatalf("after apply rows=%d want 3", n)
	}
	pos2, err := db2.WALPosition()
	if err != nil {
		t.Fatalf("WALPosition db2: %v", err)
	}
	if pos2 != end {
		t.Fatalf("replica LSN=%d want %d (must align with master)", pos2, end)
	}

	// 幂等边界：空增量不改变任何东西
	if err := db2.ApplyWALDelta(nil, true, true); err != nil {
		t.Fatalf("empty delta: %v", err)
	}
	if n := countRows(t, db2, "ship_t"); n != 3 {
		t.Fatalf("after empty delta rows=%d want 3", n)
	}
}

// TestReplicationRejectsCorruptDelta 复制流不允许"尽力而为"：
// 任何一帧 CRC 不符都必须整体拒绝，且不能部分应用。
func TestReplicationRejectsCorruptDelta(t *testing.T) {
	SetLogMute(true)
	dirA := t.TempDir()
	cfgA := walTestConfig(dirA)
	cfgA.WALCompress = "on"
	db1, err := NewDB(cfgA)
	if err != nil {
		t.Fatalf("db1: %v", err)
	}
	mustRun(t, db1, "CREATE TABLE corrupt_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db1, "INSERT INTO corrupt_t VALUES (1,10)")
	pos, _ := db1.WALPosition()
	mustRun(t, db1, "INSERT INTO corrupt_t VALUES (2,20)")
	db1.Close()

	walA := filepath.Join(dirA, "wal")
	st, _ := os.Stat(walA)
	delta := readFileRange(t, walA, pos, st.Size())
	// 翻掉最后一个字节：帧内容变了，CRC 必不符
	bad := append([]byte(nil), delta...)
	bad[len(bad)-1] ^= 0xff

	dirB := t.TempDir()
	if err := copyWALPrefix(walA, filepath.Join(dirB, "wal"), pos); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	db2, err := NewDB(walTestConfig(dirB))
	if err != nil {
		t.Fatalf("db2: %v", err)
	}
	defer db2.Close()
	if err := db2.ApplyWALDelta(bad, true, true); err == nil {
		t.Fatal("corrupt delta must be rejected")
	}
	if n := countRows(t, db2, "corrupt_t"); n != 1 {
		t.Fatalf("corrupt delta must not apply partially: rows=%d want 1", n)
	}
}

// TestReplicaReadOnlyAndPromote 从库只读，PROMOTE 后恢复写入。
func TestReplicaReadOnlyAndPromote(t *testing.T) {
	SetLogMute(true)
	// 先用一个正常实例建表并塞一行，作为从库的初始状态
	dir := t.TempDir()
	seed, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustRun(t, seed, "CREATE TABLE ro_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, seed, "INSERT INTO ro_t VALUES (1,10)")
	seed.Close()

	cfg := walTestConfig(dir)
	cfg.ReplicaOf = "http://127.0.0.1:1" // 不可达即可，本测试不关心拉取
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("replica: %v", err)
	}
	defer db.Close()

	if !db.replicaReadOnly() {
		t.Fatal("replica must be read-only before promote")
	}
	// 读允许
	if n := countRows(t, db, "ro_t"); n != 1 {
		t.Fatalf("replica read rows=%d want 1", n)
	}
	// 写拒绝，且映射到 MySQL 1290
	_, _, _, _, werr := db.runSQL("INSERT INTO ro_t VALUES (2,20)")
	if werr == nil {
		t.Fatal("write on replica must fail")
	}
	if code := mysqlErrno(werr); code != 1290 {
		t.Fatalf("read-only errno=%d want 1290 (%v)", code, werr)
	}
	// PROMOTE 解除只读
	if _, _, _, _, err := db.runSQL("PROMOTE"); err != nil {
		t.Fatalf("PROMOTE: %v", err)
	}
	if db.replicaReadOnly() {
		t.Fatal("promoted node must accept writes")
	}
	mustRun(t, db, "INSERT INTO ro_t VALUES (2,20)")
	if n := countRows(t, db, "ro_t"); n != 2 {
		t.Fatalf("after promote rows=%d want 2", n)
	}
}

// TestReplicationWALHandler 主库 HTTP 拉取端点：鉴权 + 偏移 + 原样字节。
func TestReplicationWALHandler(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	cfg.ReplicationToken = "repl-secret"
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer db.Close()
	mustRun(t, db, "CREATE TABLE h_t (id INT PRIMARY KEY, v INT)")
	pos, _ := db.WALPosition()
	mustRun(t, db, "INSERT INTO h_t VALUES (1,10)")
	end, err := db.WALPosition()
	if err != nil {
		t.Fatalf("position: %v", err)
	}

	fetch := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/replication/wal?offset="+itoa(int(pos)), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		db.handleReplicationWAL(rec, req)
		return rec
	}

	if rec := fetch("wrong"); rec.Code != 401 {
		t.Fatalf("wrong token: status=%d want 401", rec.Code)
	}
	rec := fetch("repl-secret")
	if rec.Code != 200 {
		t.Fatalf("valid token: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-WAL-End"); got != itoa64(end) {
		t.Fatalf("X-WAL-End=%s want %d", got, end)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected non-empty WAL body")
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "octet-stream") {
		t.Fatalf("content-type=%q", rec.Header().Get("Content-Type"))
	}
}

// TestReplicaLoopEndToEnd 用 httptest 扮演主库，验证轮询循环真的能把
// 增量拉下来并应用（这是唯一在单进程里能覆盖 startReplicaLoop 的方式）。
func TestReplicaLoopEndToEnd(t *testing.T) {
	SetLogMute(true)
	dirA := t.TempDir()
	db1, err := NewDB(walTestConfig(dirA))
	if err != nil {
		t.Fatalf("db1: %v", err)
	}
	mustRun(t, db1, "CREATE TABLE loop_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db1, "INSERT INTO loop_t VALUES (1,10)")
	pos, _ := db1.WALPosition()
	mustRun(t, db1, "INSERT INTO loop_t VALUES (2,20)")
	mustRun(t, db1, "INSERT INTO loop_t VALUES (3,30)")
	db1.Close()

	walA := filepath.Join(dirA, "wal")
	st, _ := os.Stat(walA)
	end := st.Size()
	delta := readFileRange(t, walA, pos, end)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		w.Header().Set("X-WAL-End", itoa64(end))
		w.Header().Set("X-WAL-Checksum", "true")
		w.Header().Set("X-WAL-Version", strconv.Itoa(WALVersion6))
		if off == pos {
			_, _ = w.Write(delta)
			return
		}
		// 已追平（或偏移不符）：返回空，模拟主库没有新增量
		w.Header().Set("X-WAL-End", itoa64(off))
	}))
	defer srv.Close()

	dirB := t.TempDir()
	if err := copyWALPrefix(walA, filepath.Join(dirB, "wal"), pos); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfgB := walTestConfig(dirB)
	cfgB.ReplicaOf = srv.URL
	cfgB.ReplicaToken = "t"
	cfgB.ReplicaPollMS = 20
	db2, err := NewDB(cfgB)
	if err != nil {
		t.Fatalf("db2: %v", err)
	}
	defer db2.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, db2, "loop_t") == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := countRows(t, db2, "loop_t"); n != 3 {
		st := db2.ReplicaStatus()
		t.Fatalf("replica rows=%d want 3 (status=%v)", n, st)
	}
	if got := db2.ReplicaStatus()["status"]; got != replicaRunning {
		t.Fatalf("replica status=%v want running", got)
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
