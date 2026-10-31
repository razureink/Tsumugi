package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestMaxScanRowsGuard 校验超大结果集不会把内存吃光，而是给出明确错误。
// 这是极端小内存设备上的关键保护：SQL 路径会把结果整体物化成 [][]interface{}。
func TestMaxScanRowsGuard(t *testing.T) {
	SetLogMute(true)
	prev := maxScanRows
	maxScanRows = 100
	t.Cleanup(func() { maxScanRows = prev })

	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE big (id INT PRIMARY KEY, v INT)")
	var sb strings.Builder
	sb.WriteString("INSERT INTO big VALUES ")
	for i := 1; i <= 500; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,%d)", i, i)
	}
	mustExec(t, db, sb.String())

	// 无 LIMIT：必须报错而不是返回 500 行
	_, _, _, _, err := db.runSQL("SELECT * FROM big")
	if err == nil {
		t.Fatal("expected scan-limit error for oversized result set")
	}
	if !strings.Contains(err.Error(), "LIMIT") {
		t.Fatalf("error should hint LIMIT, got: %v", err)
	}

	// 带 LIMIT：正常返回
	_, rows, _, _, err := db.runSQL("SELECT * FROM big LIMIT 10")
	if err != nil {
		t.Fatalf("LIMIT query failed: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("rows=%d want 10", len(rows))
	}

	// 条件查询命中少量行：不应触发上限
	_, rows, _, _, err = db.runSQL("SELECT * FROM big WHERE id <= 50")
	if err != nil {
		t.Fatalf("small result query failed: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("rows=%d want 50", len(rows))
	}

	// 上限内的全表扫描仍然放行
	maxScanRows = 600
	_, rows, _, _, err = db.runSQL("SELECT * FROM big")
	if err != nil {
		t.Fatalf("scan under the cap failed: %v", err)
	}
	if len(rows) != 500 {
		t.Fatalf("rows=%d want 500", len(rows))
	}
}

// TestMaxScanRowsByMemoryTier 校验小内存设备会自动把上限调低。
func TestMaxScanRowsByMemoryTier(t *testing.T) {
	saveTuningState(t)
	prev := maxScanRows
	t.Cleanup(func() { maxScanRows = prev })

	cases := []struct {
		ram  uint64
		want int
	}{
		{192 << 20, 50000},
		{512 << 20, 200000},
		{2 << 30, 1000000},
		{32 << 30, 2000000},
	}
	for _, c := range cases {
		maxScanRows = 2000000
		walWriteBufBytes = 1 << 20
		walBatchMaxBytes = 4 << 20
		replaySegmentBytes = 8 << 20
		replaySegmentOps = 65536
		replayWorkers = 16
		replayPipeline = true
		applyMemoryBudget(c.ram)
		if maxScanRows != c.want {
			t.Fatalf("ram=%s maxScanRows=%d want %d", fmtBytes(c.ram), maxScanRows, c.want)
		}
	}
}
