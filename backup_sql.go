package main

// ==================== 逻辑备份（mysqldump 风格）+ PITR ====================
// 现有 Backup() 只是把 WAL 文件复制一份，能用于"整个实例回滚到备份时刻"，
// 但有两个缺口：
//   1. 不能导出成可读、可迁移、可跨版本的 SQL 文本（换机器/换引擎/人工核对都要它）；
//   2. 不能在两次备份之间指定"恢复到某个时间点"（PITR）。
//
// 本文件补上这两块：
//   - DumpSQL/RestoreSQL：模式 + 数据的 SQL 快照，可读、可 diff、可跨实例导入；
//   - WALPosition/RestorePointInTime：按 WAL 字节偏移（LSN）做前缀回放，
//     即"恢复到这个偏移之前的状态"。
//
// 关于 PITR 的边界，这里必须说清楚（不糊弄）：
//   - 偏移量以当前 WAL 文件为准，**下一次 Compact 会重写 WAL，旧偏移随即失效**；
//   - 因此正确的用法是"备份时记录偏移 → 备份文件与偏移一起保存 → 恢复时用同一份
//     WAL 副本"；本模块的 RestorePointInTime 就是按这个前提写的（它读的是副本）。
//   - TTL 过期行不会进逻辑备份（它们本来就不该被恢复）。

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DumpSQL 把整个实例导出为 SQL 文本（模式 + 数据 + 视图）。
// 输出顺序固定（表名、索引名、视图名都排序），同一份数据多次导出结果稳定，
// 便于 diff 与校验。
func (db *DB) DumpSQL(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	fmt.Fprintf(bw, "-- Tsumugi logical dump\n")
	fmt.Fprintf(bw, "-- Generated at %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(bw, "-- Restore with: POST /api/admin/restore, or db.RestoreSQL(...)\n\n")

	names := make([]string, 0, 32)
	db.tables.Range(func(k, _ interface{}) bool {
		if s, ok := k.(string); ok {
			names = append(names, s)
		}
		return true
	})
	sort.Strings(names)

	views := []viewDef(nil)
	if globalViews != nil {
		views = globalViews.list()
	}

	// 先删视图：视图依赖表，重建时必须先撤掉旧定义。
	for _, v := range views {
		fmt.Fprintf(bw, "DROP VIEW IF EXISTS %s;\n", quoteQualified(v.Name))
	}
	if len(views) > 0 {
		fmt.Fprintln(bw)
	}

	// 任何带 schema 前缀的表都需要先有库。
	schemas := map[string]bool{}
	for _, n := range names {
		if i := strings.IndexByte(n, '.'); i > 0 {
			schemas[n[:i]] = true
		}
	}
	for _, n := range views {
		if i := strings.IndexByte(n.Name, '.'); i > 0 {
			schemas[n.Name[:i]] = true
		}
	}
	if len(schemas) > 0 {
		schemaNames := make([]string, 0, len(schemas))
		for s := range schemas {
			schemaNames = append(schemaNames, s)
		}
		sort.Strings(schemaNames)
		for _, s := range schemaNames {
			fmt.Fprintf(bw, "CREATE DATABASE IF NOT EXISTS %s;\n", quoteIdent(s))
		}
		fmt.Fprintln(bw)
	}

	now := time.Now().UnixNano()
	for _, name := range names {
		t := db.getTable(name)
		if t == nil {
			continue
		}
		meta := t.Meta()
		fmt.Fprintf(bw, "DROP TABLE IF EXISTS %s;\n", quoteQualified(name))
		fmt.Fprintf(bw, "%s;\n", renderCreateTable(meta, name))
		if err := dumpTableRows(bw, t, meta, name, now); err != nil {
			return err
		}
	}

	// 视图最后建：此时所有基表已就位。
	if len(views) > 0 {
		fmt.Fprintln(bw)
		for _, v := range views {
			fmt.Fprintf(bw, "CREATE OR REPLACE VIEW %s AS %s;\n", quoteQualified(v.Name), v.SQL)
		}
	}
	return bw.Flush()
}

// dumpTableRows 按主键顺序把一张表的数据写成批量 INSERT。
// 每 100 行一条语句：既压缩文件体积，也避免单条语句过长。
func dumpTableRows(bw *bufio.Writer, t *Table, meta *TableMeta, name string, now int64) error {
	const rowsPerInsert = 100

	cols := make([]string, len(meta.Fields))
	for i, f := range meta.Fields {
		cols[i] = quoteIdent(f.Name)
	}
	prefix := "INSERT INTO " + quoteQualified(name) + " (" + strings.Join(cols, ", ") + ") VALUES "

	vals := make([]interface{}, len(meta.Fields))
	var sb strings.Builder
	pending := 0
	var writeErr error

	flush := func() {
		if pending == 0 || writeErr != nil {
			return
		}
		sb.WriteString(";\n")
		if _, err := bw.WriteString(sb.String()); err != nil {
			writeErr = err
		}
		pending = 0
	}

	t.pkTree.scanRangeStop(nil, nil, func(_ int64, data []byte) bool {
		// TTL 过期行不导出：恢复时它们本就不可见。
		if e := decodeExpireAt(data); e > 0 && now > e {
			return true
		}
		decodeRowFlat(meta, data, vals)
		if pending == 0 {
			sb.Reset()
			sb.WriteString(prefix)
		} else {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for i, f := range meta.Fields {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(sqlLiteral(vals[i], f))
		}
		sb.WriteByte(')')
		pending++
		if pending == rowsPerInsert {
			flush()
		}
		return writeErr == nil
	})
	flush()
	return writeErr
}

// renderCreateTable 生成与 parseCreateTable 兼容的建表语句。
func renderCreateTable(meta *TableMeta, name string) string {
	defs := make([]string, 0, len(meta.Fields)+1+len(meta.Indexes))
	for _, f := range meta.Fields {
		defs = append(defs, quoteIdent(f.Name)+" "+sqlColumnType(f))
	}
	defs = append(defs, "PRIMARY KEY ("+quoteIdent(meta.PK)+")")
	idxNames := make([]string, 0, len(meta.Indexes))
	for n := range meta.Indexes {
		idxNames = append(idxNames, n)
	}
	sort.Strings(idxNames)
	for _, n := range idxNames {
		defs = append(defs, "KEY "+quoteIdent(n)+" ("+quoteIdent(meta.Indexes[n])+")")
	}
	return "CREATE TABLE " + quoteQualified(name) + " (" + strings.Join(defs, ", ") + ")"
}

// sqlColumnType 把字段定义映射回 DDL 类型。
// DECIMAL 只存了 scale（精度不存），用 18 位整数部分兜底，保证 scale 精确还原。
func sqlColumnType(f Field) string {
	switch f.Type {
	case TypeVarchar:
		if f.Len > 0 {
			return "VARCHAR(" + strconv.Itoa(f.Len) + ")"
		}
		return "VARCHAR(255)"
	case TypeDecimal:
		if f.Len > 0 {
			return "DECIMAL(18," + strconv.Itoa(f.Len) + ")"
		}
		return "DECIMAL"
	default:
		return fieldTypeName(f.Type)
	}
}

// sqlLiteral 把内部值渲染成可被本引擎解析回同一内部值的字面量。
// 关键点：
//   - DATE/DATETIME 内部是 Unix 纳秒，coerceTimeNanos(int64) 原样接受，直接用整数；
//   - DECIMAL 内部是放大 10^scale 的整数，必须写成带小数点的文本，
//     否则 coerceDecimalScaled 会再乘一次 10^scale（双重放大）；
//   - 字符串按 MySQL 默认转义规则引号化。
func sqlLiteral(v interface{}, f Field) string {
	if v == nil {
		return "NULL"
	}
	switch f.Type {
	case TypeInt, TypeDate, TypeDatetime:
		return strconv.FormatInt(coerceInt(v), 10)
	case TypeDecimal:
		return decimalText(coerceInt(v), f.Len)
	case TypeBool:
		if b, ok := v.(bool); ok && b {
			return "TRUE"
		}
		return "FALSE"
	default:
		return quoteSQLString(valueString(v))
	}
}

// decimalText 把 scaled（实际值 × 10^scale）还原成 "123.45" 形式。
func decimalText(scaled int64, scale int) string {
	if scale <= 0 {
		return strconv.FormatInt(scaled, 10)
	}
	neg := scaled < 0
	u := uint64(scaled)
	if neg {
		u = -u // uint64 取负不会溢出，包括 INT64_MIN
	}
	s := strconv.FormatUint(u, 10)
	for len(s) <= scale {
		s = "0" + s
	}
	out := s[:len(s)-scale] + "." + s[len(s)-scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// quoteSQLString 输出单引号字符串字面量，转义规则与 tokenizeSQL/scanQuoted 对齐：
// 反斜杠、单引号、NUL、换行、回车、制表、退格、Ctrl-Z。
func quoteSQLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			b.WriteString("''")
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 8:
			b.WriteString(`\b`)
		case 26:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// quoteIdent 用反引号引用标识符（MySQL 风格；内含反引号时用双反引号转义）。
func quoteIdent(s string) string {
	if !needsQuoting(s) {
		return s
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// quoteQualified 逐段引用 db.table 形式的名字。
func quoteQualified(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 {
		return quoteIdent(s[:i]) + "." + quoteIdent(s[i+1:])
	}
	return quoteIdent(s)
}

// RestoreSQL 执行 DumpSQL 生成的脚本（也接受外部 mysqldump 风格脚本）。
// 逐条执行，遇到第一条错误立即返回并带上语句首行，便于定位。
func (db *DB) RestoreSQL(script string) error {
	for i, stmt := range splitSQLStatements(script) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, _, _, _, err := db.runSQL(stmt); err != nil {
			return fmt.Errorf("restore statement %d (%s): %w", i+1, firstLine(stmt), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// splitSQLStatements 按顶层分号切分脚本。
// 引号/反引号内的分号、注释里的分号都不会误切。
func splitSQLStatements(script string) []string {
	var out []string
	start := 0
	i := 0
	n := len(script)
	for i < n {
		c := script[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < n {
				if script[i] == '\\' && q != '`' {
					i += 2
					continue
				}
				if script[i] == q {
					if i+1 < n && script[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '-' && i+2 <= n && script[i:i+2] == "--" &&
			(i+2 == n || script[i+2] == ' ' || script[i+2] == '\t' || script[i+2] == '\n' || script[i+2] == '\r'):
			for i < n && script[i] != '\n' {
				i++
			}
		case c == '#':
			for i < n && script[i] != '\n' {
				i++
			}
		case c == '/' && i+2 <= n && script[i:i+2] == "/*":
			i += 2
			for i+1 < n && !(script[i] == '*' && script[i+1] == '/') {
				i++
			}
			i += 2
			if i > n {
				i = n
			}
		case c == ';':
			out = append(out, script[start:i])
			start = i + 1
			i++
		default:
			i++
		}
	}
	if rest := script[start:]; strings.TrimSpace(rest) != "" {
		out = append(out, rest)
	}
	return out
}

// WALPosition 返回 WAL 当前的逻辑末尾偏移（字节）。恢复 PITR 时用它记录"现在"。
// 调用会先冲刷 WAL 缓冲，保证返回值对应的数据确实在文件里。
func (db *DB) WALPosition() (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.flushWAL(); err != nil {
		return 0, err
	}
	st, err := db.walFile.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// RestorePointInTime 把 srcWal 的前 upTo 字节复制到 dstDir 并打开该副本，
// 得到"upTo 偏移之前"的数据库状态。upTo<=0 表示整个文件。
//
// 前提：本进程内不能同时开着另一个 DB 实例（引擎的 WAL 写协程是包级单例）。
// 典型用法是"停服 → 恢复 → 启动"，或测试里先 Close 再恢复。
// 调用方负责 Close 返回的 DB。
func RestorePointInTime(cfg *Config, srcWal, dstDir string, upTo int64) (*DB, error) {
	if cfg == nil || srcWal == "" || dstDir == "" {
		return nil, fmt.Errorf("restore: cfg, srcWal and dstDir are required")
	}
	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return nil, err
	}
	in, err := os.Open(srcWal)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return nil, err
	}
	end := st.Size()
	if upTo > 0 && upTo < end {
		end = upTo
	}
	headerLen := int64(len(WALMagic) + 4 + 4)
	if end < headerLen {
		return nil, fmt.Errorf("restore: WAL prefix %d bytes is shorter than the header (%d)", end, headerLen)
	}

	walName := cfg.WALFile
	if walName == "" {
		walName = "wal"
	}
	dstWal := filepath.Join(dstDir, walName)
	out, err := os.OpenFile(dstWal, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(out, in, end); err != nil {
		out.Close()
		os.Remove(dstWal)
		return nil, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dstWal)
		return nil, err
	}
	if err := out.Close(); err != nil {
		os.Remove(dstWal)
		return nil, err
	}

	c2 := *cfg
	c2.WALDir = dstDir
	c2.BackupDir = filepath.Join(dstDir, "backup")
	return NewDB(&c2)
}

// copyWALPrefix 把 src 的前 upTo 字节复制到 dstPath（upTo<=0 表示全部）。
// 与 RestorePointInTime 的区别：它只落盘，不打开 DB，所以可在服务运行中调用。
func copyWALPrefix(src, dstPath string, upTo int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	end := st.Size()
	if upTo > 0 && upTo < end {
		end = upTo
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, in, end); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// requireUnderDir 要求 dest 位于 base 目录之内（含 base 本身）。
// 由 HTTP 传入的路径必须过这一关，防止管理接口被用来往任意位置写文件。
func requireUnderDir(base, dest string) error {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseAbs, destAbs)
	if err != nil {
		return fmt.Errorf("path %q is not under %q", dest, base)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path %q is outside %q", dest, base)
	}
	return nil
}
