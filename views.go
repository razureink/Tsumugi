package main

// ==================== C2：视图（VIEW） ====================
// MySQL 的视图是"命名的 SELECT"：读视图时把它的查询当派生表执行。
// 这里采用同样的语义，实现方式是**查询时物化**（把视图的 SELECT 跑一遍，
// 结果当作虚拟表参与外层的计划），因此：
//   - 不需要在存储层维护视图数据，天然不会与基表脱节；
//   - 代价每次读视图都要重跑它自己的查询（简单、正确、可预期）。
//
// 持久化：与 users.json / 权限文件同级的独立 JSON（原子写），
// 不写进 WAL —— 视图定义是元数据，不参与行级重放。
//
// 明确不支持（会报错而不是假装支持）：物化视图、可更新视图
// （INSERT/UPDATE/DELETE 视图）、CHECK OPTION、列别名列表。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// viewDef 一条视图定义。
type viewDef struct {
	Name string `json:"name"` // 物理全名（可能带 db. 前缀，小写）
	SQL  string `json:"sql"`  // 视图的 SELECT 语句原文
}

type viewStore struct {
	mu    sync.RWMutex
	views map[string]viewDef
	path  string
}

var globalViews *viewStore

// initViewStore 从磁盘加载视图定义（不存在则视为空）。
func initViewStore(dataDir string) {
	path := filepath.Join(dataDir, "views.json")
	vs := &viewStore{views: make(map[string]viewDef), path: path}
	if data, err := os.ReadFile(path); err == nil {
		var list []viewDef
		if json.Unmarshal(data, &list) == nil {
			for _, v := range list {
				vs.views[strings.ToLower(v.Name)] = v
			}
		}
	}
	globalViews = vs
}

// save 原子落盘（0600：视图定义可能含表名等敏感信息）。
func (vs *viewStore) save() error {
	vs.mu.RLock()
	list := make([]viewDef, 0, len(vs.views))
	for _, v := range vs.views {
		list = append(list, v)
	}
	vs.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := vs.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, vs.path)
}

// setView 建立或替换视图定义。
func (vs *viewStore) setView(name, sql string) error {
	vs.mu.Lock()
	vs.views[strings.ToLower(name)] = viewDef{Name: strings.ToLower(name), SQL: sql}
	vs.mu.Unlock()
	return vs.save()
}

// dropView 删除视图，返回是否存在。
func (vs *viewStore) dropView(name string) (bool, error) {
	key := strings.ToLower(name)
	vs.mu.Lock()
	_, ok := vs.views[key]
	if ok {
		delete(vs.views, key)
	}
	vs.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, vs.save()
}

// lookup 按全名查视图；key 需已是小写并做过 schema 限定。
func (vs *viewStore) lookup(name string) (viewDef, bool) {
	if vs == nil {
		return viewDef{}, false
	}
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	v, ok := vs.views[strings.ToLower(name)]
	return v, ok
}

// list 返回全部视图（按名字排序，供 SHOW FULL TABLES 等使用）。
func (vs *viewStore) list() []viewDef {
	if vs == nil {
		return nil
	}
	vs.mu.RLock()
	out := make([]viewDef, 0, len(vs.views))
	for _, v := range vs.views {
		out = append(out, v)
	}
	vs.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolveView 判断某个物理表名是否其实是视图，返回它的 SELECT。
// 外层查询需要把它当虚拟表处理，因此这里同时给出查询文本。
func (db *DB) resolveView(physicalName string) (viewDef, bool) {
	if globalViews == nil {
		return viewDef{}, false
	}
	return globalViews.lookup(physicalName)
}

// materializeView 执行视图自身的 SELECT，返回列名与行。
// 递归深度有上限，避免视图 A 引用 B、B 又引用 A 造成无限递归。
func (db *DB) materializeView(v viewDef, depth int) ([]string, [][]interface{}, error) {
	const maxViewDepth = 8
	if depth > maxViewDepth {
		return nil, nil, errViewTooDeep
	}
	// 视图定义里不允许出现非 SELECT 语句（写入阶段已校验，这里再兜一层）
	if !isSelectStatement(v.SQL) {
		return nil, nil, errViewNotSelect
	}
	cols, rows, _, _, err := db.runSQLSchemaAs("", v.SQL, nil, 0, viewSchemaOf(v.Name))
	if err != nil {
		return nil, nil, err
	}
	return cols, rows, nil
}

// viewSchemaOf 从视图全名推断所属库（未限定则为空，走默认库）。
func viewSchemaOf(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return ""
}

// isSelectStatement 判断语句是否为 SELECT（允许前导空白与括号外注释）。
func isSelectStatement(sql string) bool {
	return strings.EqualFold(firstWord(sql), "SELECT")
}

var (
	errViewTooDeep  = errStr("view nesting too deep (max 8 levels)")
	errViewNotSelect = errStr("view definition must be a SELECT statement")
)

// errStr 简易错误类型（避免为两个错误引入 errors.New 的包级变量散落）。
type errStr string

func (e errStr) Error() string { return string(e) }

// ---------------- SQL 语句解析 ----------------

// parseCreateView 处理 CREATE [OR REPLACE] VIEW name AS SELECT ...
// 只接受 SELECT；已存在的视图必须显式 OR REPLACE 才能覆盖（与 MySQL 一致）。
func (db *DB) parseCreateView(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // CREATE
	orReplace := false
	if p.matchKeyword("OR") {
		if !p.matchKeyword("REPLACE") {
			err = errStr("expected REPLACE after OR")
			return
		}
		orReplace = true
	}
	if !p.matchKeyword("VIEW") {
		err = errStr("expected VIEW")
		return
	}
	p.matchKeyword("IF") // 容忍 IF NOT EXISTS（语义上视作普通创建）
	p.matchKeyword("NOT")
	p.matchKeyword("EXISTS")

	nameTok := p.next()
	if nameTok.kind != kindIdent && nameTok.kind != kindStr {
		err = errStr("expected view name")
		return
	}
	phys := db.qualifyTableIn(nameTok.val, p.curSchema(db))

	// 视图名不能与已有表冲突（MySQL 里视图与表共享命名空间）
	if db.getTable(phys) != nil {
		err = errStr("table already exists: " + nameTok.val)
		return
	}
	if _, exists := globalViews.lookup(phys); exists && !orReplace {
		err = errStr("view already exists: " + nameTok.val)
		return
	}

	// AS 之后到语句末尾全部是视图定义
	if !p.matchKeyword("AS") {
		err = errStr("expected AS in CREATE VIEW")
		return
	}
	// 从当前 token 位置取回原始 SQL 文本（保留写法，重放时行为一致）
	body := p.remainingSQL()
	if !isSelectStatement(body) {
		err = errStr("view definition must be a SELECT statement")
		return
	}
	// 先执行一次，确保定义合法（列名可解析、表存在），避免存下坏视图
	if _, _, e := db.materializeView(viewDef{Name: phys, SQL: body}, 0); e != nil {
		err = fmt.Errorf("invalid view definition: %w", e)
		return
	}
	if e := globalViews.setView(phys, body); e != nil {
		err = e
		return
	}
	rawMsg = "Query OK"
	return
}

// parseDropView 处理 DROP VIEW [IF EXISTS] name
func (db *DB) parseDropView(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // DROP
	p.next() // VIEW
	ifExists := false
	if p.matchKeyword("IF") {
		ifExists = p.matchKeyword("EXISTS")
	}
	nameTok := p.next()
	if nameTok.kind != kindIdent && nameTok.kind != kindStr {
		err = errStr("expected view name")
		return
	}
	phys := db.qualifyTableIn(nameTok.val, p.curSchema(db))
	ok, e := globalViews.dropView(phys)
	if e != nil {
		err = e
		return
	}
	if !ok && !ifExists {
		err = errStr("view not found: " + nameTok.val)
		return
	}
	rawMsg = "Query OK"
	return
}

// remainingSQL 把 parser 当前位置到语句末尾的 token 重新拼成 SQL 文本。
// 用于保存视图定义：tokenizer 已经解开了引号与转义，这里必须重新加回去，
// 否则含空格/单引号的字面量会被破坏。
func (p *sqlParser) remainingSQL() string {
	var sb strings.Builder
	for i := p.i; i < len(p.toks); i++ {
		t := p.toks[i]
		switch t.kind {
		case kindEof:
			return strings.TrimSpace(sb.String())
		case kindStr:
			sb.WriteByte('\'')
			sb.WriteString(strings.ReplaceAll(t.val, "'", "''"))
			sb.WriteByte('\'')
		case kindIdent:
			if needsQuoting(t.val) {
				sb.WriteByte('`')
				sb.WriteString(strings.ReplaceAll(t.val, "`", "``"))
				sb.WriteByte('`')
			} else {
				sb.WriteString(t.val)
			}
		default:
			sb.WriteString(t.val)
		}
		sb.WriteByte(' ')
	}
	return strings.TrimSpace(sb.String())
}

// needsQuoting 判断标识符是否需要反引号包裹（保留字或含特殊字符）。
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if !(r == '_' || r == '$' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return true
		}
	}
	return false
}

// inferColumnType 从结果集推断某列的类型（取第一个非 nil 值）。
// 视图是物化出来的虚拟表，列类型必须贴近真实值，否则外层按字符串比较会漏行。
func inferColumnType(rows [][]interface{}, col int) FieldType {
	for _, r := range rows {
		if col >= len(r) || r[col] == nil {
			continue
		}
		switch r[col].(type) {
		case int64, int:
			return TypeInt
		case bool:
			return TypeBool
		default:
			return TypeVarchar
		}
	}
	return TypeVarchar
}
