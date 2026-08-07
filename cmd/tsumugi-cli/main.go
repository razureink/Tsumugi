// tsumugi-cli — Tsumugi 命令行管理工具。
//
// 通过原生二进制协议（高性能）连接 Tsumugi 服务，提供交互式 REPL、
// 数据库管理命令（CREATE/DROP/SHOW/USE DATABASE）、管理命令与批量导入。
//
// 用法：
//
//	tsumugi-cli [-h host] [-p port] [-u user] [-P pass] [-e "sql"] [-f script.sql]
//
// 也支持环境变量 TSUMUGI_HOST / TSUMUGI_PORT / TSUMUGI_USER / TSUMUGI_PASS。
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cmdAuth     = 10
	cmdPing     = 11
	cmdQuerySQL = 35
	cmdBackup   = 56
	cmdStatus   = 57
	cmdCompact  = 58
	cmdSetDura  = 59

	respOK    = 1
	respErr   = 2
	respRows  = 5
	respValue = 3
)

type client struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

type options struct {
	host string
	port int
	user string
	pass string
	exec string // -e SQL
	file string // -f 脚本
}

func parseArgs() options {
	o := options{
		host: envOr("TSUMUGI_HOST", "127.0.0.1"),
		port: atoiOr(envOr("TSUMUGI_PORT", "9999"), 9999),
		user: envOr("TSUMUGI_USER", "root"),
		pass: envOr("TSUMUGI_PASS", "password"),
	}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "-h", "--host":
			o.host = next()
		case "-p", "--port":
			o.port = atoiOr(next(), 9999)
		case "-u", "--user":
			o.user = next()
		case "-P", "-pwd", "--pass":
			o.pass = next()
		case "-e", "--exec":
			o.exec = next()
		case "-f", "--file":
			o.file = next()
		}
	}
	return o
}

func main() {
	o := parseArgs()
	// 非交互单次命令：-e 执行 SQL；-f 执行脚本
	if o.exec != "" || o.file != "" {
		c, err := connect(o)
		if err != nil {
			fail("connect: %v", err)
		}
		defer c.conn.Close()
		if o.exec != "" {
			runStatements(c, strings.TrimSpace(o.exec))
			return
		}
		runFile(c, o.file)
		return
	}
	repl(o)
}

// ---- 连接与协议 ----

func connect(o options) (*client, error) {
	addr := net.JoinHostPort(o.host, fmt.Sprintf("%d", o.port))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c := &client{conn: conn, br: bufio.NewReaderSize(conn, 32*1024), bw: bufio.NewWriterSize(conn, 32*1024)}
	if err := c.auth(o.user, o.pass); err != nil {
		conn.Close()
		return nil, err
	}
	// 恢复正常超时（长时间交互）
	conn.SetDeadline(time.Time{})
	return c, nil
}

func (c *client) auth(user, pass string) error {
	var buf bytes.Buffer
	buf.WriteByte(cmdAuth)
	writeLenStr(&buf, user)
	writeLenStr(&buf, pass)
	if _, err := c.bw.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}
	r := make([]byte, 1)
	if _, err := io.ReadFull(c.br, r); err != nil {
		return err
	}
	if r[0] == respErr {
		return readErr(c.br)
	}
	return nil
}

// execSQL 发送 SQL 并返回 (columns, rows, affected, rawMsg, err)。
func (c *client) execSQL(sql string) ([]string, [][]string, int64, string, error) {
	var buf bytes.Buffer
	buf.WriteByte(cmdQuerySQL)
	writeLenStr(&buf, sql)
	if _, err := c.bw.Write(buf.Bytes()); err != nil {
		return nil, nil, 0, "", err
	}
	if err := c.bw.Flush(); err != nil {
		return nil, nil, 0, "", err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.br, code); err != nil {
		return nil, nil, 0, "", err
	}
	switch code[0] {
	case respErr:
		return nil, nil, 0, "", readErr(c.br)
	case respOK:
		var affected uint64
		if err := binary.Read(c.br, binary.BigEndian, &affected); err != nil {
			return nil, nil, 0, "", err
		}
		var msgLen uint16
		if err := binary.Read(c.br, binary.BigEndian, &msgLen); err != nil {
			return nil, nil, 0, "", err
		}
		msg := make([]byte, msgLen)
		io.ReadFull(c.br, msg)
		return nil, nil, int64(affected), string(msg), nil
	case respRows:
		var ncol uint32
		if err := binary.Read(c.br, binary.BigEndian, &ncol); err != nil {
			return nil, nil, 0, "", err
		}
		cols := make([]string, ncol)
		for i := uint32(0); i < ncol; i++ {
			var e error
			cols[i], e = readLenStr(c.br)
			if e != nil {
				return nil, nil, 0, "", e
			}
		}
		var nrow uint32
		if err := binary.Read(c.br, binary.BigEndian, &nrow); err != nil {
			return nil, nil, 0, "", err
		}
		rows := make([][]string, 0, nrow)
		for i := uint32(0); i < nrow; i++ {
			var vc uint32
			if err := binary.Read(c.br, binary.BigEndian, &vc); err != nil {
				return nil, nil, 0, "", err
			}
			row := make([]string, vc)
			for j := uint32(0); j < vc; j++ {
				var sl uint32
				if err := binary.Read(c.br, binary.BigEndian, &sl); err != nil {
					return nil, nil, 0, "", err
				}
				b := make([]byte, sl)
				if _, err := io.ReadFull(c.br, b); err != nil {
					return nil, nil, 0, "", err
				}
				row[j] = string(b)
			}
			rows = append(rows, row)
		}
		return cols, rows, 0, "", nil
	default:
		return nil, nil, 0, "", fmt.Errorf("unexpected response %d", code[0])
	}
}

// simpleCmd 发送无参命令（如 backup/compact），返回错误或 nil。
func (c *client) simpleCmd(cmd byte) error {
	if _, err := c.bw.Write([]byte{cmd}); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.br, code); err != nil {
		return err
	}
	if code[0] == respErr {
		return readErr(c.br)
	}
	return nil
}

// status 查询服务状态，返回 JSON 快照。
func (c *client) status() (string, error) {
	if _, err := c.bw.Write([]byte{cmdStatus}); err != nil {
		return "", err
	}
	if err := c.bw.Flush(); err != nil {
		return "", err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.br, code); err != nil {
		return "", err
	}
	if code[0] == respErr {
		return "", readErr(c.br)
	}
	if code[0] != respValue {
		return "", fmt.Errorf("unexpected response %d", code[0])
	}
	var l uint32
	if err := binary.Read(c.br, binary.BigEndian, &l); err != nil {
		return "", err
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(c.br, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// setDurability 切换持久化模式。
func (c *client) setDurability(mode string) error {
	var buf bytes.Buffer
	buf.WriteByte(cmdSetDura)
	writeLenStr(&buf, mode)
	if _, err := c.bw.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.br, code); err != nil {
		return err
	}
	if code[0] == respErr {
		return readErr(c.br)
	}
	return nil
}

// ---- 协议辅助 ----
func writeLenStr(buf *bytes.Buffer, s string) {
	binary.Write(buf, binary.BigEndian, uint16(len(s)))
	buf.WriteString(s)
}

func readLenStr(r io.Reader) (string, error) {
	var l uint16
	if err := binary.Read(r, binary.BigEndian, &l); err != nil {
		return "", err
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func readErr(r io.Reader) error {
	var l uint32
	if err := binary.Read(r, binary.BigEndian, &l); err != nil {
		return err
	}
	b := make([]byte, l)
	io.ReadFull(r, b)
	return fmt.Errorf("%s", string(b))
}

// ---- 展示 ----

// renderTable 以 MySQL 风格绘制结果集。
func renderTable(cols []string, rows [][]string) {
	if len(cols) == 0 {
		return
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for _, r := range rows {
		for i := range cols {
			if i < len(r) && len(r[i]) > widths[i] {
				widths[i] = len(r[i])
			}
		}
	}
	line := func(cells []string) string {
		var sb strings.Builder
		sb.WriteString("|")
		for i, c := range cells {
			w := widths[i] + 2
			sb.WriteString(" " + c + strings.Repeat(" ", w-len(c)-1) + "|")
		}
		return sb.String()
	}
	sep := func() string {
		var sb strings.Builder
		sb.WriteString("+")
		for _, w := range widths {
			sb.WriteString(strings.Repeat("-", w+2) + "+")
		}
		return sb.String()
	}
	fmt.Println(sep())
	fmt.Println(line(cols))
	fmt.Println(sep())
	for _, r := range rows {
		fmt.Println(line(r))
	}
	fmt.Println(sep())
	fmt.Printf("%d rows in set\n", len(rows))
}

// ---- 交互 REPL ----

func repl(o options) {
	c, err := connect(o)
	if err != nil {
		fail("connect: %v", err)
	}
	defer c.conn.Close()

	fmt.Printf("Welcome to Tsumugi CLI\nType 'help' or 'exit'.\n\n")
	sc := bufio.NewScanner(os.Stdin)
	hist := loadHistory()
	idx := len(hist) // 暂未实现上下翻历史
	_ = idx
	for {
		fmt.Print("tsumugi> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-") {
			line = strings.TrimPrefix(line, "-")
		}
		switch {
		case line == "exit" || line == "quit" || line == "q":
			fmt.Println("Bye")
			return
		case line == "help":
			printHelp()
			continue
		}
		// 追加历史
		if line != "exit" && line != "quit" && line != "q" && line != "help" {
			hist = append(hist, line)
			saveHistory(hist)
		}
		if dispatchLine(c, line) {
			fmt.Println()
			continue
		}
		runOne(c, line)
		fmt.Println()
	}
}

// dispatchLine 处理管理命令（status/compact/backup/set durability/import）。
// 返回 true 表示已作为管理命令处理。
func dispatchLine(c *client, line string) bool {
	switch {
	case line == "status":
		runStatus(c)
		return true
	case line == "compact":
		if err := c.simpleCmd(cmdCompact); err != nil {
			fmt.Printf("ERROR: %v\n", err)
		} else {
			fmt.Println("WAL flushed / compacted")
		}
		return true
	case line == "backup":
		if err := c.simpleCmd(cmdBackup); err != nil {
			fmt.Printf("ERROR: %v\n", err)
		} else {
			fmt.Println("backup completed")
		}
		return true
	case strings.HasPrefix(line, "set durability "):
		mode := strings.TrimSpace(strings.TrimPrefix(line, "set durability "))
		if err := c.setDurability(mode); err != nil {
			fmt.Printf("ERROR: %v\n", err)
		} else {
			fmt.Printf("durability set to %s\n", mode)
		}
		return true
	case strings.HasPrefix(line, "import "):
		runImport(c, line)
		return true
	}
	return false
}

func printHelp() {
	fmt.Println("Commands:")
	fmt.Println("  <SQL>                       执行任意 SQL（SELECT/CREATE/INSERT/USE...）")
	fmt.Println("  status                      显示服务状态（QPS/TPS/内存/磁盘）")
	fmt.Println("  compact                     触发 WAL 整理")
	fmt.Println("  backup                      触发备份")
	fmt.Println("  set durability <batch|fsync> 切换持久化模式")
	fmt.Println("  import --table T --file F.csv [--db D]  批量导入 CSV")
	fmt.Println("  help / exit                 帮助 / 退出")
	fmt.Println("数据库管理：SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE")
}

// runStatements 按分号拆分执行多条语句（忽略引号内的分号）。
func runStatements(c *client, sql string) {
	var parts []string
	var cur strings.Builder
	inStr := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\'' {
			inStr = !inStr
		}
		if ch == ';' && !inStr {
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		parts = append(parts, s)
	}
	for _, p := range parts {
		if p == "" {
			continue
		}
		if dispatchLine(c, p) {
			continue
		}
		runOne(c, p)
	}
}

// runOne 执行一条 SQL 并展示结果。
func runOne(c *client, sql string) {
	cols, rows, affected, rawMsg, err := c.execSQL(sql)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	if cols != nil {
		renderTable(cols, rows)
		return
	}
	if rawMsg != "" {
		fmt.Println(rawMsg)
	} else {
		fmt.Printf("Query OK, %d row(s) affected\n", affected)
	}
}

// runStatus 显示服务状态（QPS/TPS/内存/磁盘）。
func runStatus(c *client) {
	raw, err := c.status()
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		fmt.Println(raw)
		return
	}
	ms := func(k string) string { return fmt.Sprintf("%v", m[k]) }
	fmt.Printf("commands: %-10s errors: %-10s qps: %s   tps: %s\n", ms("total_commands"), ms("total_errors"), ms("qps"), ms("tps"))
	fmt.Printf("cpu: %s%%   mem: %s MB   goroutines: %s   durability: %s\n", ms("cpu_percent"), ms("mem_mb"), ms("goroutines"), ms("durability"))
	fmt.Printf("wal: %s MB written\n", ms("wal_total_mb"))
}

// runImport 从 CSV 文件批量导入到指定表。
// 用法：import --table users --file data.csv [--db myapp]
// CSV 第一行为列名，其余行为数据；主键必须在其中。
func runImport(c *client, line string) {
	var table, file, db string
	args := strings.Fields(line)
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--table", "-t":
			if i+1 < len(args) {
				i++
				table = args[i]
			}
		case "--file", "-f":
			if i+1 < len(args) {
				i++
				file = args[i]
			}
		case "--db", "-d":
			if i+1 < len(args) {
				i++
				db = args[i]
			}
		}
	}
	if table == "" || file == "" {
		fmt.Println("usage: import --table <table> --file <data.csv> [--db <database>]")
		return
	}
	if db != "" {
		if _, _, _, _, err := c.execSQL("USE " + db); err != nil {
			fmt.Printf("ERROR: USE %s: %v\n", db, err)
			return
		}
	}
	f, err := os.Open(file)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	defer f.Close()
	br := bufio.NewReader(f)
	// 跳过 UTF-8 BOM
	if peek, _ := br.Peek(3); len(peek) == 3 && peek[0] == 0xEF && peek[1] == 0xBB && peek[2] == 0xBF {
		br.Discard(3)
	}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		fmt.Printf("ERROR: read header: %v\n", err)
		return
	}
	// 找主键：从 CREATE TABLE 提取不现实，用表的第一列约定或全列值方式。这里用列名直接构造 INSERT。
	ok := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			return
		}
		cols := make([]string, 0, len(header))
		vals := make([]string, 0, len(header))
		for i, h := range header {
			cols = append(cols, h)
			vals = append(vals, quoteCSV(rec[i]))
		}
		sql := "INSERT INTO " + table + " (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(vals, ",") + ")"
		if _, _, _, _, err := c.execSQL(sql); err != nil {
			fmt.Printf("ERROR at line %d: %v\n", ok+2, err)
			return
		}
		ok++
		if ok%1000 == 0 {
			fmt.Printf("  ...%d rows\n", ok)
		}
	}
	fmt.Printf("imported %d rows into %s\n", ok, table)
}

func quoteCSV(s string) string {
	// 数值保持原样，其余加单引号（并转义内部单引号）
	n := true
	for _, c := range s {
		if (c < '0' || c > '9') && c != '-' && c != '.' {
			n = false
			break
		}
	}
	if s == "" {
		return "''"
	}
	if n {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runFile(c *client, path string) {	f, err := os.Open(path)
	if err != nil {
		fail("open: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var stmt strings.Builder
	flush := func() {
		if s := strings.TrimSpace(stmt.String()); s != "" {
			runOne(c, s)
		}
		stmt.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		stmt.WriteString(line)
		stmt.WriteString("\n")
		if strings.Contains(strings.TrimSpace(stmt.String()), ";") {
			flush()
		}
	}
	flush()
}

// ---- 历史（~/.tsumugi_history）----

func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tsumugi_history"
	}
	return filepath.Join(home, ".tsumugi_history")
}

func loadHistory() []string {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func saveHistory(hist []string) {
	if len(hist) > 1000 {
		hist = hist[len(hist)-1000:]
	}
	f, err := os.OpenFile(historyPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, l := range hist {
		fmt.Fprintln(f, l)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "tsumugi-cli: "+format+"\n", args...)
	os.Exit(1)
}
