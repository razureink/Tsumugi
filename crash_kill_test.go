package main

// ==================== kill -9 故障演练 ====================
// 本机没有 -race，也不能可靠地模拟"断电"，但可以模拟**进程被强杀**：
// 子进程调 os.Exit(1) 不走 defer/Close，等价于 kill -9（OS 页缓存保留，
// 但进程内未 flush 的 bufio 缓冲丢失）。
//
// 断言的核心是数据库最基本的承诺：
//   1. 被强杀后 WAL 仍能重放，数据库能正常启动（不能因为尾部未完成而拒绝启动）；
//   2. 所有"已确认"的写入（调用过 WALPosition 即已 flush+fsync）一条都不能丢；
//   3. 未确认的尾部写入允许丢失，但**不允许出现部分行/损坏行**；
//   4. 恢复后的数据库还能继续正常读写。
//
// 用 -short 时跳过（默认全量跑）。

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

const (
	crashHelperEnv = "TSUMUGI_CRASH_HELPER"
	crashDirEnv    = "TSUMUGI_CRASH_DIR"
	crashAcked     = 40  // 已确认并落盘的行数
	crashUnacked   = 40  // 确认之后继续写、但随时可能被强杀的行数
)

// TestCrashKill9Child 只在被父进程以 helper 模式拉起时执行。
func TestCrashKill9Child(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		fmt.Fprintln(os.Stderr, "missing crash dir")
		os.Exit(2)
	}
	SetLogMute(true)
	cfg := walTestConfig(dir)
	cfg.Durability = DuraFsync
	db, err := NewDB(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewDB: %v\n", err)
		os.Exit(2)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE ck (id INT PRIMARY KEY, v INT)"); err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(2)
	}
	for i := 1; i <= crashAcked; i++ {
		if _, _, _, _, err := db.runSQL(
			fmt.Sprintf("INSERT INTO ck VALUES (%d,%d)", i, i*10)); err != nil {
			fmt.Fprintf(os.Stderr, "insert %d: %v\n", i, err)
			os.Exit(2)
		}
		// WALPosition 会 flush+fsync：到这里这条写入才算"已确认"。
		if _, err := db.WALPosition(); err != nil {
			fmt.Fprintf(os.Stderr, "sync %d: %v\n", i, err)
			os.Exit(2)
		}
	}
	// 父进程据此判断哪些行必须存活。
	fmt.Fprintf(os.Stdout, "ACKED=%d\n", crashAcked)
	os.Stdout.Sync()

	// 继续写，但不落盘；随后被强杀，这些行允许丢失。
	for i := crashAcked + 1; i <= crashAcked+crashUnacked; i++ {
		if _, _, _, _, err := db.runSQL(
			fmt.Sprintf("INSERT INTO ck VALUES (%d,%d)", i, i*10)); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced insert %d: %v\n", i, err)
			break
		}
	}
	// kill -9：不执行任何 defer / Close。
	os.Exit(1)
}

var ackedRe = regexp.MustCompile(`(?m)^ACKED=(\d+)$`)

// TestCrashKill9Loop 反复强杀并重放，验证"已确认写入不丢、未确认尾部不损坏"。
func TestCrashKill9Loop(t *testing.T) {
	if testing.Short() {
		t.Skip("kill drill skipped in -short")
	}
	const iterations = 12
	for iter := 0; iter < iterations; iter++ {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashKill9Child$")
		cmd.Env = append(os.Environ(),
			crashHelperEnv+"=1", crashDirEnv+"="+dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("iter %d: 子进程本应被强杀却正常退出", iter)
		}
		m := ackedRe.FindSubmatch(out)
		if m == nil {
			t.Fatalf("iter %d: 未找到 ACKED 标记，输出:\n%s", iter, out)
		}
		acked, _ := strconv.Atoi(string(m[1]))
		if acked != crashAcked {
			t.Fatalf("iter %d: acked=%d want %d", iter, acked, crashAcked)
		}

		// 用同一目录重开：这一步证明 WAL 尾部（可能不完整）不会让启动失败。
		db, err := NewDB(walTestConfig(dir))
		if err != nil {
			t.Fatalf("iter %d: 强杀后无法恢复启动: %v\n--- child output ---\n%s", iter, err, out)
		}
		func() {
			defer db.Close()
			rows := rowsOf(t, db, "SELECT COUNT(*) FROM ck")
			if len(rows) != 1 {
				t.Fatalf("iter %d: COUNT 返回 %v", iter, rows)
			}
			got, _ := rows[0][0].(int64)
			if got < int64(acked) {
				t.Fatalf("iter %d: 只恢复 %d 行，已确认 %d 行丢失", iter, got, acked)
			}
			if got > int64(crashAcked+crashUnacked) {
				t.Fatalf("iter %d: 恢复了 %d 行，超过写入总数（数据重复）", iter, got)
			}
			// 已确认行必须逐条存在且值正确（不允许部分行）。
			for i := 1; i <= acked; i++ {
				got := rowsOf(t, db, fmt.Sprintf("SELECT v FROM ck WHERE id = %d", i))
				if len(got) != 1 || got[0][0] != int64(i*10) {
					t.Fatalf("iter %d: 已确认行 id=%d 丢失或损坏: %v", iter, i, got)
				}
			}
			// 恢复后的实例必须还能写：WAL 没有被尾部残帧卡住。
			probe := int64(10000 + iter)
			if _, _, _, _, err := db.runSQL(
				fmt.Sprintf("INSERT INTO ck VALUES (%d, 7)", probe)); err != nil {
				t.Fatalf("iter %d: 恢复后无法继续写入: %v", iter, err)
			}
			if got := rowsOf(t, db, fmt.Sprintf("SELECT v FROM ck WHERE id = %d", probe)); len(got) != 1 {
				t.Fatalf("iter %d: 恢复后写入不可见", iter)
			}
		}()
	}
}
