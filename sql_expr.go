package main

// ==================== SQL 表达式层 ====================
// 覆盖 MySQL 常用表达式语法：列引用、字面量、函数调用、CASE、运算符、
// IN / LIKE / BETWEEN / IS NULL，以及聚合函数（COUNT/SUM/AVG/MIN/MAX）。
//
// 设计原则：**不改造现有快路径**。简单等值/范围条件仍走字节级谓词
// （selectRBytes / pkTree 点查），本层只服务"通用路径"：
//   表达式投影、JOIN、GROUP BY/HAVING、DISTINCT、复杂 WHERE。
// 行数据以 map[string]interface{} 表示，键为小写列名；JOIN 时同时保留
// "别名.列" 形式的限定键。

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type exprKind uint8

const (
	exConst exprKind = iota
	exCol
	exStar
	exFunc
	exAgg
	exCase
)

type expr struct {
	kind  exprKind
	val   interface{}
	table string  // exCol 限定符（别名/表名）
	col   string  // exCol 列名
	fn    string  // 函数名（大写）
	args  []*expr // 参数
	when  []*expr // CASE：成对 (条件, 结果)
	els   *expr   // CASE ELSE
	dist  bool    // COUNT(DISTINCT x)
	alias string  // 输出别名
	raw   string  // 无别名时的输出列名
}

// exprHasAgg 判断表达式树里是否包含聚合函数。
func exprHasAgg(e *expr) bool {
	if e == nil {
		return false
	}
	if e.kind == exAgg {
		return true
	}
	for _, a := range e.args {
		if exprHasAgg(a) {
			return true
		}
	}
	for _, w := range e.when {
		if exprHasAgg(w) {
			return true
		}
	}
	return exprHasAgg(e.els)
}

// isAggName 判断是否为聚合函数。
func isAggName(fn string) bool {
	switch fn {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		return true
	}
	return false
}

// ---------- 解析 ----------

func (p *sqlParser) parseExpr() (*expr, error) { return p.parseOr() }

func (p *sqlParser) parseOr() (*expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().is("OR") || p.peek().is("||") {
		p.next()
		right, e := p.parseAnd()
		if e != nil {
			return nil, e
		}
		left = &expr{kind: exFunc, fn: "OR", args: []*expr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseAnd() (*expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.peek().is("AND") || p.peek().is("&&") {
		p.next()
		right, e := p.parseNot()
		if e != nil {
			return nil, e
		}
		left = &expr{kind: exFunc, fn: "AND", args: []*expr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseNot() (*expr, error) {
	if p.peek().is("NOT") {
		p.next()
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &expr{kind: exFunc, fn: "NOT", args: []*expr{inner}}, nil
	}
	return p.parseCmp()
}

// parseCmp 解析比较/谓词；无法识别为谓词时退化为加法表达式。
func (p *sqlParser) parseCmp() (*expr, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	neg := false
	if p.peek().is("NOT") {
		// NOT LIKE / NOT IN / NOT BETWEEN / NOT REGEXP
		save := p.i
		p.next()
		if t := p.peek(); t.is("LIKE") || t.is("IN") || t.is("BETWEEN") || t.is("REGEXP") {
			neg = true
		} else {
			p.i = save
			return left, nil
		}
	}
	t := p.peek()
	switch {
	case t.is("LIKE"):
		p.next()
		rhs, e := p.parseAdd()
		if e != nil {
			return nil, e
		}
		fn := "LIKE"
		if neg {
			fn = "NOTLIKE"
		}
		return &expr{kind: exFunc, fn: fn, args: []*expr{left, rhs}}, nil
	case t.is("REGEXP"):
		p.next()
		rhs, e := p.parseAdd()
		if e != nil {
			return nil, e
		}
		fn := "REGEXP"
		if neg {
			fn = "NOTREGEXP"
		}
		return &expr{kind: exFunc, fn: fn, args: []*expr{left, rhs}}, nil
	case t.is("IN"):
		p.next()
		if !p.matchSym("(") {
			return nil, fmt.Errorf("expected ( after IN")
		}
		var items []*expr
		if !p.peek().is(")") {
			for {
				it, e := p.parseExpr()
				if e != nil {
					return nil, e
				}
				items = append(items, it)
				if !p.matchSym(",") {
					break
				}
			}
		}
		if !p.matchSym(")") {
			return nil, fmt.Errorf("expected ) after IN list")
		}
		fn := "IN"
		if neg {
			fn = "NOTIN"
		}
		return &expr{kind: exFunc, fn: fn, args: append([]*expr{left}, items...)}, nil
	case t.is("BETWEEN"):
		p.next()
		lo, e := p.parseAdd()
		if e != nil {
			return nil, e
		}
		if !p.matchKeyword("AND") {
			return nil, fmt.Errorf("expected AND in BETWEEN")
		}
		hi, e := p.parseAdd()
		if e != nil {
			return nil, e
		}
		fn := "BETWEEN"
		if neg {
			fn = "NOTBETWEEN"
		}
		return &expr{kind: exFunc, fn: fn, args: []*expr{left, lo, hi}}, nil
	case t.is("IS"):
		p.next()
		isNot := p.matchKeyword("NOT")
		if !p.matchKeyword("NULL") {
			return nil, fmt.Errorf("expected NULL after IS")
		}
		if isNot {
			return &expr{kind: exFunc, fn: "ISNOTNULL", args: []*expr{left}}, nil
		}
		return &expr{kind: exFunc, fn: "ISNULL", args: []*expr{left}}, nil
	}
	// 关系运算符（tokenizer 会把 >= <= <> != 拆成两个符号）
	op, ok := p.readCmpOp()
	if !ok {
		return left, nil
	}
	rhs, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	return &expr{kind: exFunc, fn: op, args: []*expr{left, rhs}}, nil
}

// readCmpOp 读取（并合并）比较运算符。
func (p *sqlParser) readCmpOp() (string, bool) {
	t := p.peek()
	if t.kind != kindSym {
		return "", false
	}
	switch t.val {
	case "=", ">", "<":
		if n := p.toks[p.i+1]; n.kind == kindSym && n.val == "=" {
			p.next()
			p.next()
			return t.val + "=", true
		}
		if t.val == "<" && p.toks[p.i+1].kind == kindSym && p.toks[p.i+1].val == ">" {
			p.next()
			p.next()
			return "<>", true
		}
		p.next()
		return t.val, true
	case "!":
		if n := p.toks[p.i+1]; n.kind == kindSym && n.val == "=" {
			p.next()
			p.next()
			return "<>", true
		}
	}
	return "", false
}

func (p *sqlParser) parseAdd() (*expr, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != kindSym || (t.val != "+" && t.val != "-") {
			return left, nil
		}
		p.next()
		right, e := p.parseMul()
		if e != nil {
			return nil, e
		}
		left = &expr{kind: exFunc, fn: t.val, args: []*expr{left, right}}
	}
}

func (p *sqlParser) parseMul() (*expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == kindSym && (t.val == "*" || t.val == "/" || t.val == "%") {
			p.next()
			right, e := p.parseUnary()
			if e != nil {
				return nil, e
			}
			left = &expr{kind: exFunc, fn: t.val, args: []*expr{left, right}}
			continue
		}
		if t.is("DIV") || t.is("MOD") {
			p.next()
			right, e := p.parseUnary()
			if e != nil {
				return nil, e
			}
			left = &expr{kind: exFunc, fn: t.upper, args: []*expr{left, right}}
			continue
		}
		return left, nil
	}
}

func (p *sqlParser) parseUnary() (*expr, error) {
	if t := p.peek(); t.kind == kindSym && (t.val == "-" || t.val == "+") {
		p.next()
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if t.val == "-" {
			return &expr{kind: exFunc, fn: "NEG", args: []*expr{inner}}, nil
		}
		return inner, nil
	}
	return p.parsePrimary()
}

func (p *sqlParser) parsePrimary() (*expr, error) {
	t := p.peek()
	if t.kind == kindSym && t.val == "(" {
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.matchSym(")") {
			return nil, fmt.Errorf("expected )")
		}
		return inner, nil
	}
	if t.is("CASE") {
		return p.parseCase()
	}
	if t.is("CAST") || t.is("CONVERT") {
		return p.parseCast()
	}
	// 函数调用 / 聚合
	if (t.kind == kindIdent || t.kind == kindKw) && p.toks[p.i+1].kind == kindSym && p.toks[p.i+1].val == "(" {
		name := strings.ToUpper(t.val)
		p.next()
		p.next() // (
		e := &expr{kind: exFunc, fn: name}
		if isAggName(name) {
			e.kind = exAgg
		}
		if p.peek().is("DISTINCT") {
			p.next()
			e.dist = true
		}
		if p.peek().is("*") && name == "COUNT" {
			p.next()
			e.args = []*expr{{kind: exStar}}
		} else if !p.peek().is(")") {
			for {
				arg, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				e.args = append(e.args, arg)
				if !p.matchSym(",") {
					break
				}
			}
		}
		if !p.matchSym(")") {
			return nil, fmt.Errorf("expected ) after %s(", name)
		}
		if e.kind == exAgg && len(e.args) == 0 {
			e.args = []*expr{{kind: exStar}}
		}
		return e, nil
	}
	if t.kind == kindIdent || t.kind == kindKw {
		// 布尔/NULL 字面量优先当常量，否则会被当成列名
		switch strings.ToUpper(t.val) {
		case "NULL":
			p.next()
			return &expr{kind: exConst, val: nil}, nil
		case "TRUE":
			p.next()
			return &expr{kind: exConst, val: true}, nil
		case "FALSE":
			p.next()
			return &expr{kind: exConst, val: false}, nil
		}
		name := t.val
		p.next()
		if p.peek().kind == kindSym && p.peek().val == "." {
			p.next()
			part := p.next()
			if part.kind != kindIdent && part.kind != kindKw && part.kind != kindStr {
				return nil, fmt.Errorf("expected column after %s.", name)
			}
			return &expr{kind: exCol, table: name, col: part.val, raw: name + "." + part.val}, nil
		}
		return &expr{kind: exCol, col: name, raw: name}, nil
	}
	if t.kind == kindSym && t.val == "*" {
		p.next()
		return &expr{kind: exStar}, nil
	}
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return &expr{kind: exConst, val: v}, nil
}

func (p *sqlParser) parseCase() (*expr, error) {
	p.next() // CASE
	e := &expr{kind: exCase}
	if !p.peek().is("WHEN") {
		base, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		e.args = []*expr{base}
	}
	for p.matchKeyword("WHEN") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.matchKeyword("THEN") {
			return nil, fmt.Errorf("expected THEN in CASE")
		}
		res, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		e.when = append(e.when, cond, res)
	}
	if p.matchKeyword("ELSE") {
		els, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		e.els = els
	}
	if !p.matchKeyword("END") {
		return nil, fmt.Errorf("expected END in CASE")
	}
	return e, nil
}

func (p *sqlParser) parseCast() (*expr, error) {
	name := strings.ToUpper(p.next().val)
	if !p.matchSym("(") {
		return nil, fmt.Errorf("expected ( after %s", name)
	}
	inner, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	// 消费类型名（AS type / , type）
	if p.matchKeyword("AS") || p.matchSym(",") {
		if p.peek().kind == kindSym && p.peek().val == "(" {
			p.next()
			for !p.peek().is(")") && p.peek().kind != kindEof {
				p.next()
			}
			p.matchSym(")")
		} else {
			p.next()
		}
	}
	if !p.matchSym(")") {
		return nil, fmt.Errorf("expected ) after %s", name)
	}
	typ := name
	if typ == "CONVERT" {
		typ = "SIGNED"
	}
	return &expr{kind: exFunc, fn: "CAST_" + typ, args: []*expr{inner}}, nil
}

// ---------- 求值 ----------

func lowerKey(s string) string { return strings.ToLower(s) }

func (e *expr) eval(row map[string]interface{}) interface{} {
	if e == nil {
		return nil
	}
	switch e.kind {
	case exConst:
		return e.val
	case exStar:
		return int64(1)
	case exCol:
		// 行 map 可能保留原始大小写（快路径 SelectR/decodeRow），也可能已小写（JOIN 合并行）
		if e.table != "" {
			if v, ok := row[e.table+"."+e.col]; ok {
				return v
			}
			if v, ok := row[lowerKey(e.table+"."+e.col)]; ok {
				return v
			}
		}
		if v, ok := row[e.col]; ok {
			return v
		}
		if v, ok := row[lowerKey(e.col)]; ok {
			return v
		}
		return nil
	case exCase:
		return e.evalCase(row)
	case exFunc:
		return e.evalFunc(row)
	}
	return nil
}

func (e *expr) evalCase(row map[string]interface{}) interface{} {
	if len(e.args) == 1 { // CASE base WHEN v THEN r
		base := e.args[0].eval(row)
		for i := 0; i+1 < len(e.when); i += 2 {
			if sqlValueEqual(base, e.when[i].eval(row)) {
				return e.when[i+1].eval(row)
			}
		}
	} else {
		for i := 0; i+1 < len(e.when); i += 2 {
			if truthy(e.when[i].eval(row)) {
				return e.when[i+1].eval(row)
			}
		}
	}
	if e.els != nil {
		return e.els.eval(row)
	}
	return nil
}

// evalGroup 在分组上下文求值：聚合对整个分组计算（按 expr 指针缓存）。
func (e *expr) evalGroup(row map[string]interface{}, group []map[string]interface{}, memo map[*expr]interface{}) interface{} {
	if e == nil {
		return nil
	}
	if e.kind == exAgg {
		if v, ok := memo[e]; ok {
			return v
		}
		v := e.computeAgg(group)
		memo[e] = v
		return v
	}
	if e.kind == exCase || e.kind == exFunc {
		savedArgs, savedWhen, savedEls := e.args, e.when, e.els
		e.args = mapExprSlice(e.args, func(x *expr) *expr { return substituteAgg(x, row, group, memo) })
		e.when = mapExprSlice(e.when, func(x *expr) *expr { return substituteAgg(x, row, group, memo) })
		e.els = substituteAgg(e.els, row, group, memo)
		var v interface{}
		if e.kind == exCase {
			v = e.evalCase(row)
		} else {
			v = e.evalFunc(row)
		}
		e.args, e.when, e.els = savedArgs, savedWhen, savedEls
		return v
	}
	return e.eval(row)
}

func mapExprSlice(in []*expr, f func(*expr) *expr) []*expr {
	if in == nil {
		return nil
	}
	out := make([]*expr, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func substituteAgg(e *expr, row map[string]interface{}, group []map[string]interface{}, memo map[*expr]interface{}) *expr {
	if e == nil {
		return nil
	}
	if e.kind == exAgg {
		return &expr{kind: exConst, val: e.evalGroup(row, group, memo)}
	}
	return e
}

func (e *expr) computeAgg(group []map[string]interface{}) interface{} {
	switch e.fn {
	case "COUNT":
		if len(e.args) == 1 && e.args[0].kind == exStar {
			return int64(len(group))
		}
		if e.dist {
			seen := make(map[string]struct{}, len(group))
			for _, r := range group {
				v := e.args[0].eval(r)
				if v == nil {
					continue
				}
				seen[fmt.Sprint(v)] = struct{}{}
			}
			return int64(len(seen))
		}
		var n int64
		for _, r := range group {
			if e.args[0].eval(r) != nil {
				n++
			}
		}
		return n
	case "SUM", "AVG":
		var sum float64
		var n int64
		for _, r := range group {
			if f, ok := toFloat(e.args[0].eval(r)); ok {
				sum += f
				n++
			}
		}
		if n == 0 {
			return nil
		}
		if e.fn == "AVG" {
			return sum / float64(n)
		}
		return sum
	case "MIN", "MAX":
		var best interface{}
		for _, r := range group {
			v := e.args[0].eval(r)
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c := cmpVal(v, best)
			if (e.fn == "MIN" && c < 0) || (e.fn == "MAX" && c > 0) {
				best = v
			}
		}
		return best
	}
	return nil
}

func (e *expr) evalFunc(row map[string]interface{}) interface{} {
	// 短路求值的逻辑运算符
	switch e.fn {
	case "AND":
		for _, a := range e.args {
			if !truthy(a.eval(row)) {
				return false
			}
		}
		return true
	case "OR":
		for _, a := range e.args {
			if truthy(a.eval(row)) {
				return true
			}
		}
		return false
	case "NOT":
		if len(e.args) != 1 {
			return nil
		}
		v := e.args[0].eval(row)
		if v == nil {
			return nil
		}
		return !truthy(v)
	case "ISNULL":
		if len(e.args) != 1 {
			return nil
		}
		return e.args[0].eval(row) == nil
	case "ISNOTNULL":
		if len(e.args) != 1 {
			return nil
		}
		return e.args[0].eval(row) != nil
	}
	vals := make([]interface{}, len(e.args))
	for i, a := range e.args {
		vals[i] = a.eval(row)
	}
	return evalFuncValues(e.fn, vals)
}

// evalFuncValues 对已求值参数执行函数/运算符。
func evalFuncValues(fn string, v []interface{}) interface{} {
	switch fn {
	case "+", "-", "*", "/", "%", "DIV", "MOD", "NEG":
		return arith(fn, v)
	case "=", "<>", "!=", ">", "<", ">=", "<=":
		return compareOp(fn, v)
	case "LIKE":
		return likeOp(v)
	case "NOTLIKE":
		r := likeOp(v)
		if r == nil {
			return nil
		}
		return !r.(bool)
	case "REGEXP":
		return regexpOp(v)
	case "NOTREGEXP":
		r := regexpOp(v)
		if r == nil {
			return nil
		}
		return !r.(bool)
	case "IN", "NOTIN":
		return inOp(v, fn == "NOTIN")
	case "BETWEEN", "NOTBETWEEN":
		if len(v) != 3 {
			return nil
		}
		if v[0] == nil || v[1] == nil || v[2] == nil {
			return nil
		}
		r := cmpVal(v[0], v[1]) >= 0 && cmpVal(v[0], v[2]) <= 0
		if fn == "NOTBETWEEN" {
			return !r
		}
		return r
	}
	return evalScalarFunc(fn, v)
}

func arith(fn string, v []interface{}) interface{} {
	if fn == "NEG" {
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return -f
	}
	if len(v) != 2 {
		return nil
	}
	a, ok1 := toFloat(v[0])
	b, ok2 := toFloat(v[1])
	if !ok1 || !ok2 {
		return nil
	}
	switch fn {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		if b == 0 {
			return nil
		}
		return a / b
	case "%", "MOD":
		if int64(b) == 0 {
			return nil
		}
		return float64(int64(a) % int64(b))
	case "DIV":
		if int64(b) == 0 {
			return nil
		}
		return float64(int64(a) / int64(b))
	}
	return nil
}

func compareOp(op string, v []interface{}) interface{} {
	if len(v) != 2 || v[0] == nil || v[1] == nil {
		return nil // 与 NULL 比较恒为 UNKNOWN
	}
	c := cmpVal(v[0], v[1])
	switch op {
	case "=":
		return c == 0
	case "<>", "!=":
		return c != 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	}
	return nil
}

func likeOp(v []interface{}) interface{} {
	if len(v) != 2 || v[0] == nil || v[1] == nil {
		return nil
	}
	s, ok := v[0].(string)
	pat, ok2 := v[1].(string)
	if !ok || !ok2 {
		return false
	}
	return likeMatch(s, pat)
}

var regexpCache sync.Map // pattern -> *regexp.Regexp

func regexpCompile(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexpCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexpCache.Store(pattern, re)
	return re, nil
}

func regexpOp(v []interface{}) interface{} {
	if len(v) != 2 || v[0] == nil || v[1] == nil {
		return nil
	}
	s, ok := v[0].(string)
	pat, ok2 := v[1].(string)
	if !ok || !ok2 {
		return false
	}
	re, err := regexpCompile(pat)
	if err != nil {
		return nil
	}
	return re.MatchString(s)
}

func inOp(v []interface{}, neg bool) interface{} {
	if len(v) < 1 || v[0] == nil {
		return nil
	}
	for _, item := range v[1:] {
		if item == nil {
			continue
		}
		if sqlValueEqual(v[0], item) {
			return !neg
		}
	}
	return neg
}

// ---------- 标量函数 ----------

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return float64(i), true
		}
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func truthy(v interface{}) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case string:
		f, ok := toFloat(n)
		return ok && f != 0
	}
	f, ok := toFloat(v)
	return ok && f != 0
}

func sqlValueEqual(a, b interface{}) bool {
	if a == nil || b == nil {
		return false
	}
	if af, ok := toFloat(a); ok {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		// IN 的字符串等值同样忽略大小写（IN 本质是一串 = 的并集）。
		return eqFold(as, bs)
	}
	return false
}

func toStr(v interface{}) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case bool:
		if n {
			return "1"
		}
		return "0"
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1e15 {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'g', -1, 64)
	}
	if f, ok := toFloat(v); ok {
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// coerceExprConsts 把表达式里与 DATE/DATETIME/DECIMAL 列比较的常量折算成
// 行内存储值（纳秒 / 定点整数）。表达式求值（compareOp → cmpVal）只看 Go 值，
// 没有列类型信息：不折算的话 `UPDATE t SET ... WHERE amount = 12.34` 会拿
// 行里的 1234 与 12.34 比较，永远不命中。
//
// 只处理"列 op 常量"与 BETWEEN/IN 的常量位；列与列、表达式与常量不处理
// （后者无法在编译期确定列类型）。
func coerceExprConsts(e *expr, meta *TableMeta) {
	if e == nil {
		return
	}
	if e.kind == exFunc && len(e.args) >= 2 {
		switch e.fn {
		case "=", "<>", "!=", ">", "<", ">=", "<=":
			if len(e.args) == 2 {
				for i := 0; i < 2; i++ {
					col, con := e.args[i], e.args[1-i]
					if col.kind == exCol && con.kind == exConst {
						if fi := meta.fieldIndexOf(col.col); fi >= 0 {
							con.val = coerceCondLiteral(meta.Fields[fi], con.val)
						}
					}
				}
			}
		case "BETWEEN", "NOTBETWEEN":
			if len(e.args) == 3 && e.args[0].kind == exCol {
				if fi := meta.fieldIndexOf(e.args[0].col); fi >= 0 {
					for _, con := range e.args[1:] {
						if con.kind == exConst {
							con.val = coerceCondLiteral(meta.Fields[fi], con.val)
						}
					}
				}
			}
		case "IN", "NOTIN":
			if e.args[0].kind == exCol {
				if fi := meta.fieldIndexOf(e.args[0].col); fi >= 0 {
					for _, con := range e.args[1:] {
						if con.kind == exConst {
							con.val = coerceCondLiteral(meta.Fields[fi], con.val)
						}
					}
				}
			}
		}
	}
	for _, a := range e.args {
		coerceExprConsts(a, meta)
	}
	for _, w := range e.when {
		coerceExprConsts(w, meta)
	}
	coerceExprConsts(e.els, meta)
}

func clampInt(n int64, lo, hi int64) int64 {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// 会话上下文（供 DATABASE()/USER() 等信息函数使用）：每条命令执行前由协议层设置。
var (
	sessionSchema atomic.Value // string
	sessionUser   atomic.Value // string
)

func setSessionContext(schema, user string) {
	sessionSchema.Store(schema)
	sessionUser.Store(user)
}

func currentSchemaName() string {
	if v, ok := sessionSchema.Load().(string); ok {
		return v
	}
	return "tsumugi"
}

func currentUserName() string {
	if v, ok := sessionUser.Load().(string); ok {
		return v
	}
	return "root"
}

// lastInsertIDSeq 服务端最近一次插入的 ID（MySQL 为连接级，这里近似为服务级）。
var lastInsertIDSeq int64

// evalScalarFunc 标量/信息函数分发；返回 nil 表示未知函数（调用方报错）。
func evalScalarFunc(fn string, v []interface{}) interface{} {
	switch fn {
	// ---- 字符串 ----
	case "CONCAT":
		var sb strings.Builder
		for _, x := range v {
			if x == nil {
				return nil
			}
			sb.WriteString(toStr(x))
		}
		return sb.String()
	case "CONCAT_WS":
		if len(v) == 0 {
			return nil
		}
		sep := toStr(v[0])
		parts := make([]string, 0, len(v))
		for _, x := range v[1:] {
			if x == nil {
				continue
			}
			parts = append(parts, toStr(x))
		}
		return strings.Join(parts, sep)
	case "LENGTH", "OCTET_LENGTH":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return int64(len(toStr(v[0])))
	case "CHAR_LENGTH", "CHARACTER_LENGTH":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return int64(len([]rune(toStr(v[0]))))
	case "UPPER", "UCASE":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return strings.ToUpper(toStr(v[0]))
	case "LOWER", "LCASE":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return strings.ToLower(toStr(v[0]))
	case "SUBSTRING", "SUBSTR", "MID":
		if len(v) < 2 || v[0] == nil {
			return nil
		}
		s := []rune(toStr(v[0]))
		pos, ok := toFloat(v[1])
		if !ok {
			return nil
		}
		start := int(pos) - 1
		if start < 0 {
			start = 0
		}
		if start >= len(s) {
			return ""
		}
		end := len(s)
		if len(v) >= 3 {
			l, ok := toFloat(v[2])
			if !ok {
				return nil
			}
			end = start + int(l)
			if end > len(s) {
				end = len(s)
			}
			if end < start {
				end = start
			}
		}
		return string(s[start:end])
	case "LEFT":
		if len(v) != 2 || v[0] == nil {
			return nil
		}
		s := []rune(toStr(v[0]))
		n, ok := toFloat(v[1])
		if !ok || int(n) <= 0 {
			return ""
		}
		if int(n) > len(s) {
			n = float64(len(s))
		}
		return string(s[:int(n)])
	case "RIGHT":
		if len(v) != 2 || v[0] == nil {
			return nil
		}
		s := []rune(toStr(v[0]))
		n, ok := toFloat(v[1])
		if !ok || int(n) <= 0 {
			return ""
		}
		if int(n) > len(s) {
			n = float64(len(s))
		}
		return string(s[len(s)-int(n):])
	case "TRIM":
		if len(v) == 0 || v[0] == nil {
			return nil
		}
		return strings.TrimSpace(toStr(v[0]))
	case "LTRIM":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return strings.TrimLeft(toStr(v[0]), " \t\n\r")
	case "RTRIM":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return strings.TrimRight(toStr(v[0]), " \t\n\r")
	case "REPLACE":
		if len(v) != 3 {
			return nil
		}
		return strings.ReplaceAll(toStr(v[0]), toStr(v[1]), toStr(v[2]))
	case "REVERSE":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		r := []rune(toStr(v[0]))
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r)
	case "REPEAT":
		if len(v) != 2 {
			return nil
		}
		n, ok := toFloat(v[1])
		if !ok || int(n) <= 0 {
			return ""
		}
		return strings.Repeat(toStr(v[0]), int(clampInt(int64(n), 0, 100000)))
	case "SPACE":
		if len(v) != 1 {
			return nil
		}
		n, ok := toFloat(v[0])
		if !ok || int(n) <= 0 {
			return ""
		}
		return strings.Repeat(" ", int(clampInt(int64(n), 0, 100000)))
	case "LPAD", "RPAD":
		if len(v) != 3 {
			return nil
		}
		s := []rune(toStr(v[0]))
		width, _ := toFloat(v[1])
		pad := []rune(toStr(v[2]))
		if int(width) <= len(s) {
			return string(s[:int(clampInt(int64(width), 0, int64(len(s))))])
		}
		if len(pad) == 0 {
			return string(s)
		}
		need := int(width) - len(s)
		var sb strings.Builder
		writePad := func() {
			for need > 0 {
				if need >= len(pad) {
					sb.WriteString(string(pad))
					need -= len(pad)
				} else {
					sb.WriteString(string(pad[:need]))
					need = 0
				}
			}
		}
		if fn == "LPAD" {
			writePad()
			sb.WriteString(string(s))
		} else {
			sb.WriteString(string(s))
			writePad()
		}
		return sb.String()
	case "LOCATE", "INSTR", "POSITION":
		if len(v) < 2 {
			return nil
		}
		sub, s := toStr(v[0]), toStr(v[1])
		idx := strings.Index(s, sub)
		if idx < 0 {
			return int64(0)
		}
		return int64(len([]rune(s[:idx])) + 1)
	case "ASCII":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		s := toStr(v[0])
		if s == "" {
			return int64(0)
		}
		return int64(s[0])
	case "HEX":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		if f, ok := toFloat(v[0]); ok && f == math.Trunc(f) {
			return strings.ToUpper(strconv.FormatUint(uint64(f), 16))
		}
		return strings.ToUpper(hex.EncodeToString([]byte(toStr(v[0]))))
	case "MD5":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		sum := md5.Sum([]byte(toStr(v[0])))
		return hex.EncodeToString(sum[:])
	case "SHA1", "SHA":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		sum := sha1.Sum([]byte(toStr(v[0])))
		return hex.EncodeToString(sum[:])
	case "SHA2":
		if len(v) != 2 || v[0] == nil {
			return nil
		}
		n, _ := toFloat(v[1])
		s := toStr(v[0])
		switch int(n) {
		case 224:
			sum := sha256.Sum224([]byte(s))
			return hex.EncodeToString(sum[:])
		case 256, 0:
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		case 384:
			sum := sha512.Sum384([]byte(s))
			return hex.EncodeToString(sum[:])
		case 512:
			sum := sha512.Sum512([]byte(s))
			return hex.EncodeToString(sum[:])
		}
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	// ---- 数学 ----
	case "ABS":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return math.Abs(f)
	case "CEIL", "CEILING":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return float64(math.Ceil(f))
	case "FLOOR":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return float64(math.Floor(f))
	case "ROUND":
		if len(v) < 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		digits := 0
		if len(v) >= 2 {
			d, _ := toFloat(v[1])
			digits = int(d)
		}
		p := math.Pow(10, float64(digits))
		return math.Round(f*p) / p
	case "TRUNCATE":
		if len(v) != 2 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		d, _ := toFloat(v[1])
		p := math.Pow(10, float64(int(d)))
		return math.Trunc(f*p) / p
	case "SIGN":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		switch {
		case f > 0:
			return float64(1)
		case f < 0:
			return float64(-1)
		}
		return float64(0)
	case "POW", "POWER":
		if len(v) != 2 {
			return nil
		}
		a, ok1 := toFloat(v[0])
		b, ok2 := toFloat(v[1])
		if !ok1 || !ok2 {
			return nil
		}
		return math.Pow(a, b)
	case "SQRT":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok || f < 0 {
			return nil
		}
		return math.Sqrt(f)
	case "EXP":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return math.Exp(f)
	case "LN", "LOG":
		if len(v) == 1 {
			f, ok := toFloat(v[0])
			if !ok || f <= 0 {
				return nil
			}
			return math.Log(f)
		}
		if len(v) == 2 {
			base, ok1 := toFloat(v[0])
			x, ok2 := toFloat(v[1])
			if !ok1 || !ok2 || base <= 0 || x <= 0 {
				return nil
			}
			return math.Log(x) / math.Log(base)
		}
		return nil
	case "LOG10":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok || f <= 0 {
			return nil
		}
		return math.Log10(f)
	case "LOG2":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok || f <= 0 {
			return nil
		}
		return math.Log2(f)
	case "PI":
		return math.Pi
	case "RAND":
		if len(v) == 1 {
			if seed, ok := toFloat(v[0]); ok {
				return rand.New(rand.NewSource(int64(seed))).Float64()
			}
		}
		return rand.Float64()
	case "SIN", "COS", "TAN", "ASIN", "ACOS", "ATAN", "ATAN2":
		if len(v) == 0 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		if fn == "ATAN2" {
			if len(v) != 2 {
				return nil
			}
			b, ok2 := toFloat(v[1])
			if !ok2 {
				return nil
			}
			return math.Atan2(f, b)
		}
		switch fn {
		case "SIN":
			return math.Sin(f)
		case "COS":
			return math.Cos(f)
		case "TAN":
			return math.Tan(f)
		case "ASIN":
			return math.Asin(f)
		case "ACOS":
			return math.Acos(f)
		case "ATAN":
			return math.Atan(f)
		}
	case "RADIANS":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return f * math.Pi / 180
	case "DEGREES":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return f * 180 / math.Pi
	case "GREATEST", "LEAST":
		if len(v) == 0 {
			return nil
		}
		best := v[0]
		for _, x := range v[1:] {
			if x == nil || best == nil {
				return nil
			}
			c := cmpVal(x, best)
			if (fn == "GREATEST" && c > 0) || (fn == "LEAST" && c < 0) {
				best = x
			}
		}
		return best
	case "MOD":
		return arith("MOD", v)
	// ---- 日期时间 ----
	case "NOW", "CURRENT_TIMESTAMP", "LOCALTIME", "LOCALTIMESTAMP", "SYSDATE":
		return time.Now().Format("2006-01-02 15:04:05")
	case "CURDATE", "CURRENT_DATE":
		return time.Now().Format("2006-01-02")
	case "CURTIME", "CURRENT_TIME":
		return time.Now().Format("15:04:05")
	case "UTC_TIMESTAMP":
		return time.Now().UTC().Format("2006-01-02 15:04:05")
	case "UTC_DATE":
		return time.Now().UTC().Format("2006-01-02")
	case "UTC_TIME":
		return time.Now().UTC().Format("15:04:05")
	case "UNIX_TIMESTAMP":
		if len(v) == 0 {
			return time.Now().Unix()
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		return t.Unix()
	case "FROM_UNIXTIME":
		if len(v) != 1 {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return nil
		}
		return time.Unix(int64(f), 0).Format("2006-01-02 15:04:05")
	case "DATE":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		return t.Format("2006-01-02")
	case "YEAR", "MONTH", "DAY", "DAYOFMONTH", "HOUR", "MINUTE", "SECOND", "QUARTER", "WEEK", "DAYOFWEEK", "WEEKDAY", "DAYOFYEAR", "TIME_TO_SEC":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		switch fn {
		case "YEAR":
			return int64(t.Year())
		case "MONTH":
			return int64(t.Month())
		case "DAY", "DAYOFMONTH":
			return int64(t.Day())
		case "HOUR":
			return int64(t.Hour())
		case "MINUTE":
			return int64(t.Minute())
		case "SECOND":
			return int64(t.Second())
		case "QUARTER":
			return int64((int(t.Month())-1)/3 + 1)
		case "WEEK":
			_, w := t.ISOWeek()
			return int64(w)
		case "DAYOFWEEK":
			return int64(int(t.Weekday()) + 1)
		case "WEEKDAY":
			return int64((int(t.Weekday()) + 6) % 7)
		case "DAYOFYEAR":
			return int64(t.YearDay())
		case "TIME_TO_SEC":
			return int64(t.Hour()*3600 + t.Minute()*60 + t.Second())
		}
	case "DATEDIFF":
		if len(v) != 2 {
			return nil
		}
		a, ok1 := parseSQLTime(toStr(v[0]))
		b, ok2 := parseSQLTime(toStr(v[1]))
		if !ok1 || !ok2 {
			return nil
		}
		return int64(a.Sub(b).Hours() / 24)
	case "DATE_ADD", "DATE_SUB", "ADDDATE", "SUBDATE":
		if len(v) < 2 {
			return nil
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		n, okn := toFloat(v[1])
		if !okn {
			return nil
		}
		unit := "DAY"
		if len(v) >= 3 {
			unit = strings.ToUpper(toStr(v[2]))
		}
		d := time.Duration(n) * 24 * time.Hour
		if fn == "DATE_SUB" || fn == "SUBDATE" {
			d = -d
		}
		return addInterval(t, d, unit).Format("2006-01-02 15:04:05")
	case "DATE_FORMAT":
		if len(v) != 2 || v[0] == nil {
			return nil
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		return dateFormat(t, toStr(v[1]))
	case "STR_TO_DATE":
		if len(v) != 2 {
			return nil
		}
		if t, err := time.Parse(convertDateFormat(toStr(v[1])), toStr(v[0])); err == nil {
			return t.Format("2006-01-02 15:04:05")
		}
		return nil
	case "LAST_DAY":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		t, ok := parseSQLTime(toStr(v[0]))
		if !ok {
			return nil
		}
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		return first.AddDate(0, 1, -1).Format("2006-01-02")
	// ---- 条件 / NULL ----
	case "IFNULL", "NVL":
		if len(v) != 2 {
			return nil
		}
		if v[0] != nil {
			return v[0]
		}
		return v[1]
	case "NULLIF":
		if len(v) != 2 {
			return nil
		}
		if sqlValueEqual(v[0], v[1]) {
			return nil
		}
		return v[0]
	case "COALESCE":
		for _, x := range v {
			if x != nil {
				return x
			}
		}
		return nil
	case "IF":
		if len(v) != 3 {
			return nil
		}
		if truthy(v[0]) {
			return v[1]
		}
		return v[2]
	case "CAST_SIGNED", "CAST_UNSIGNED":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return float64(0)
		}
		return float64(int64(f))
	case "CAST_CHAR":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		return toStr(v[0])
	case "CAST_DECIMAL", "CAST_DOUBLE", "CAST_FLOAT":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		f, ok := toFloat(v[0])
		if !ok {
			return float64(0)
		}
		return f
	case "CAST_DATE", "CAST_DATETIME":
		if len(v) != 1 || v[0] == nil {
			return nil
		}
		if t, ok := parseSQLTime(toStr(v[0])); ok {
			return t.Format("2006-01-02 15:04:05")
		}
		return nil
	// ---- 信息函数 ----
	case "DATABASE", "SCHEMA":
		return currentSchemaName()
	case "USER", "CURRENT_USER", "SESSION_USER", "SYSTEM_USER":
		return currentUserName() + "@localhost"
	case "VERSION":
		return "8.0.36-Tsumugi"
	case "CONNECTION_ID":
		return int64(0)
	case "LAST_INSERT_ID":
		return lastInsertIDSeq
	case "FOUND_ROWS", "ROW_COUNT":
		return int64(0)
	case "CHARSET":
		return "utf8mb4"
	case "COLLATION":
		return "utf8mb4_0900_ai_ci"
	case "COERCIBILITY":
		return int64(2)
	case "TYPEOF":
		if len(v) != 1 {
			return nil
		}
		switch v[0].(type) {
		case nil:
			return "NULL"
		case string:
			return "string"
		case bool:
			return "bigint"
		default:
			return "double"
		}
	}
	return nil
}

func addInterval(t time.Time, d time.Duration, unit string) time.Time {
	switch unit {
	case "SECOND":
		return t.Add(d * time.Second)
	case "MINUTE":
		return t.Add(d * time.Minute)
	case "HOUR":
		return t.Add(d * time.Hour)
	case "WEEK":
		return t.Add(d * 7 * 24 * time.Hour)
	case "MONTH":
		return t.AddDate(0, int(d.Hours()/24/30), 0)
	case "YEAR":
		return t.AddDate(int(d.Hours()/24/365), 0, 0)
	default:
		return t.Add(d)
	}
}

// parseSQLTime 解析常见日期/日期时间字面量。
func parseSQLTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02",
		"20060102150405", "20060102", "15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			if l == "15:04:05" {
				now := time.Now()
				return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local), true
			}
			return t, true
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(f), 0), true
	}
	return time.Time{}, false
}

func dateFormat(t time.Time, f string) string {
	r := strings.NewReplacer(
		"%Y", t.Format("2006"),
		"%y", t.Format("06"),
		"%m", t.Format("01"),
		"%c", strconv.Itoa(int(t.Month())),
		"%d", t.Format("02"),
		"%e", strconv.Itoa(t.Day()),
		"%H", t.Format("15"),
		"%i", t.Format("04"),
		"%s", t.Format("05"),
		"%p", t.Format("PM"),
		"%M", t.Format("January"),
		"%b", t.Format("Jan"),
		"%W", t.Format("Monday"),
		"%a", t.Format("Mon"),
		"%j", t.Format("002"),
		"%%", "%",
	)
	return r.Replace(f)
}

func convertDateFormat(f string) string {
	r := strings.NewReplacer("%Y", "2006", "%m", "01", "%d", "02", "%H", "15", "%i", "04", "%s", "05")
	return r.Replace(f)
}
