package main

// ==================== C5：可观测性（EXPLAIN / 慢查询日志） ====================
// 替代 MySQL 的前提之一是"能被 DBA 接手"。缺了 EXPLAIN 与慢查询日志，
// 运维遇到慢查询只能盲猜——这两项比再多一个 SQL 语法更能决定可用性。
//
// 本文件实现：
//   EXPLAIN <SELECT>   输出真实访问路径（const/ref/range/ALL + 选中的索引）
//   慢查询日志          超过阈值的语句进环形缓冲，可用 SHOW SLOWQUERIES 查看
// 说明：EXPLAIN 复用与执行器完全相同的判定逻辑（主键点查 → 二级索引等值 →
// 索引范围下推 → 全表扫描），因此"计划说的"和"实际跑的"不会脱节。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------- 慢查询日志 ----------------

// slowQueryEntry 一条慢查询记录。
type slowQueryEntry struct {
	At     time.Time
	User   string
	Dur    time.Duration
	SQL    string
	Rows   int
	Failed bool
}

// slowQueryLog 固定容量的环形缓冲：不限制语句量增长，只保留最近 N 条。
// 用固定容量是为了避免"慢查询日志本身成为内存泄漏源"（极端设备场景）。
type slowQueryLog struct {
	mu    sync.Mutex
	ring  []slowQueryEntry
	start int // 环形缓冲起始下标
	count int
	total int64 // 累计条数（可能远大于容量）
}

const slowLogCapacity = 256

var globalSlowLog = &slowQueryLog{ring: make([]slowQueryEntry, slowLogCapacity)}

// slowQueryThreshold 慢查询阈值（纳秒）。0 表示关闭记录。
var slowQueryThreshold atomic.Int64

// SetSlowQueryThreshold 设置慢查询阈值（<=0 表示关闭）。
func SetSlowQueryThreshold(d time.Duration) {
	slowQueryThreshold.Store(int64(d))
}

// recordSlowQuery 记录一条慢查询（环形覆盖最旧记录）。
// 只在超过阈值时调用，正常路径不承担额外开销。
func recordSlowQuery(user string, dur time.Duration, sql string, rows int, failed bool) {
	lg := globalSlowLog
	lg.mu.Lock()
	defer lg.mu.Unlock()
	idx := (lg.start + lg.count) % slowLogCapacity
	if lg.count == slowLogCapacity {
		// 满了：覆盖最旧的一条，起点前移
		idx = lg.start
		lg.start = (lg.start + 1) % slowLogCapacity
	} else {
		lg.count++
	}
	// SQL 文本截断，避免一条超长语句长期占内存
	const maxSQLLen = 512
	if len(sql) > maxSQLLen {
		sql = sql[:maxSQLLen] + "..."
	}
	lg.ring[idx] = slowQueryEntry{At: time.Now(), User: user, Dur: dur, SQL: sql, Rows: rows, Failed: failed}
	lg.total++
}

// snapshotSlowQueries 返回最近记录（按时间从旧到新）。
func snapshotSlowQueries() []slowQueryEntry {
	lg := globalSlowLog
	lg.mu.Lock()
	defer lg.mu.Unlock()
	out := make([]slowQueryEntry, 0, lg.count)
	for i := 0; i < lg.count; i++ {
		out = append(out, lg.ring[(lg.start+i)%slowLogCapacity])
	}
	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

// resetSlowQueries 清空（供测试与运维手动清理）。
func resetSlowQueries() {
	lg := globalSlowLog
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.start = 0
	lg.count = 0
}

// ---------------- EXPLAIN ----------------

// explainColumns 与 MySQL 的 EXPLAIN 输出列保持一致，客户端工具可直接解析。
var explainColumns = []string{
	"id", "select_type", "table", "partitions", "type",
	"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
}

// handleExplain 处理 EXPLAIN <SELECT>。只支持传统格式；FORMAT=JSON 暂不实现。
func (db *DB) handleExplain(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // EXPLAIN
	// 容忍 EXPLAIN EXTENDED / FORMAT=TRADITIONAL 这类前缀
	for {
		t := p.peek()
		if t.kind != kindIdent && t.kind != kindKw {
			break
		}
		up := strings.ToUpper(t.val)
		if up == "EXTENDED" || up == "PARTITIONS" {
			p.next()
			continue
		}
		break
	}
	// 只支持对 SELECT 做计划。
	// 注意：parseSelectPlan 期望 parser 已经位于 SELECT **之后**
	//（它第一条就是检查 DISTINCT/ALL），所以这里必须先把 SELECT 消费掉。
	t := p.peek()
	if !(t.kind == kindIdent || t.kind == kindKw) || !strings.EqualFold(t.val, "SELECT") {
		err = fmt.Errorf("EXPLAIN currently supports SELECT only")
		return
	}
	p.next() // 消费 SELECT
	pl, e := db.parseSelectPlan(p)
	if e != nil {
		err = e
		return
	}
	columns = explainColumns
	return columns, db.explainRows(pl), 0, "", nil
}

// explainRows 依据计划生成访问路径行。判定顺序必须与执行器一致：
// 主键点查 → 二级索引等值 → 索引范围下推 → 全表扫描。
func (db *DB) explainRows(pl *selectPlan) [][]interface{} {
	out := make([][]interface{}, 0, len(pl.tables))
	conds, filters, _ := extractConds(pl.where)

	for i, ft := range pl.tables {
		selectType := "SIMPLE"
		if i > 0 {
			selectType = "SIMPLE"
		}
		tbl := db.getTable(db.qualifyTable(ft.name))
		if tbl == nil {
			out = append(out, explainRow(1, selectType, ft.name, "ALL", "", "", "", "", 0, 100, "table not found"))
			continue
		}
		meta := tbl.Meta()
		tableRows := tbl.pkTree.Size()
		accessType, possibleKey, key, keyLen, ref, estRows, extra := db.planAccess(meta, tbl, conds, filters, tableRows)
		out = append(out, explainRow(1, selectType, ft.name, accessType, possibleKey, key, keyLen, ref, estRows, 100, extra))
	}
	return out
}

// planAccess 计算单表的访问路径（与执行器同样的优先级）。
func (db *DB) planAccess(meta *TableMeta, tbl *Table, conds map[string]interface{}, filters []RangeCond, tableRows int64) (accessType, possibleKey, key, keyLen, ref string, estRows int64, extra string) {
	extraParts := make([]string, 0, 2)
	if len(conds) > 0 || len(filters) > 0 {
		extraParts = append(extraParts, "Using where")
	}
	// 1) 主键点查
	if v, ok := conds[meta.PK]; ok {
		if _, isInt := v.(int64); isInt {
			return "const", "PRIMARY", "PRIMARY", "8", "const", 1, joinExtra(extraParts, "")
		}
	}
	// 2) 二级索引等值
	for idxName, fieldName := range meta.Indexes {
		if val, ok := conds[fieldName]; ok {
			possibleKey = idxName
			est := int64(1)
			if tree, ok := tbl.indexes()[idxName]; ok && tree != nil {
				if pks, found := tree.Get(tbl.encodeIndexValue(fieldName, val)); found {
					est = int64(len(pks))
				}
			}
			return "ref", idxName, idxName, "8", "const", est, joinExtra(extraParts, "")
		}
	}
	// 3) 索引列范围
	for fi := range filters {
		fieldIdx := meta.fieldIndexOf(filters[fi].Field)
		if fieldIdx < 0 {
			continue
		}
		if idxName, ok := tbl.indexNameForField(meta.Fields[fieldIdx].Name); ok {
			return "range", idxName, idxName, "8", "", estimateRangeRows(tableRows), joinExtra(extraParts, "")
		}
	}
	// 4) 全表扫描（含仅主键范围）
	if len(filters) > 0 {
		return "range", "PRIMARY", "PRIMARY", "8", "", estimateRangeRows(tableRows), joinExtra(extraParts, "")
	}
	return "ALL", "", "", "", "", tableRows, joinExtra(extraParts, "")
}


// estimateRangeRows 范围扫描的行数估算：没有统计信息时给 1/10 的粗估。
func estimateRangeRows(tableRows int64) int64 {
	if tableRows <= 10 {
		return tableRows
	}
	return tableRows / 10
}

// joinExtra 拼接 Extra 列（去空）。
func joinExtra(parts []string, add string) string {
	out := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	if add != "" {
		out = append(out, add)
	}
	return strings.Join(out, "; ")
}

// explainRow 组装一行（列顺序与 explainColumns 一致）。
func explainRow(id int, selectType, table, accessType, possibleKey, key, keyLen, ref string, rows int64, filtered int, extra string) []interface{} {
	return []interface{}{
		int64(id), selectType, table, nil, accessType,
		nullable(possibleKey), nullable(key), nullable(keyLen), nullable(ref),
		rows, int64(filtered), extra,
	}
}

// nullable 把空字符串转成 SQL NULL，便于客户端区分"没有"和"空串"。
func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// firstWords 把 SQL 截断到 n 字节用于日志（避免一条超长语句刷爆日志）。
func firstWords(sql string, n int) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) <= n {
		return sql
	}
	return sql[:n] + "..."
}

// boolToInt 便于把 bool 作为整型列返回（客户端展示更一致）。
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
