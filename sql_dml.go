package main

// ==================== UPDATE / DELETE（通用谓词） ====================
// MySQL 的 UPDATE/DELETE 允许任意 WHERE 与 SET 表达式；旧实现只支持
// `WHERE pk = 常量` 且 SET 只能写常量，ORM 与运维脚本稍复杂就会报错。
// 这里改为：主键等值条件仍走点查，其余退化为扫描 + 谓词求值。

import (
	"fmt"
	"time"
)

// scanTargets 按谓词逐行回调（where=nil 表示全表）。
// 流式处理而不是先把所有行物化成切片：大表 DELETE/UPDATE 时
// 物化会让内存凭空翻一倍。limit < 0 表示不限制，0 表示一行都不匹配。
func (t *Table) scanTargets(where *expr, limit int, fn func(pk int64, row map[string]interface{}, expireAt int64) error) error {
	meta := t.Meta()
	now := time.Now().UnixNano()
	// 先收集主键再逐行改动：回调里会 Delete/Update（写时复制发布新根），
	// 在扫描过程中改树虽然靠不可变节点勉强能走完，但语义脆弱；
	// 主键列表只有 8 字节/行，代价远小于出错风险。
	keys := make([]int64, 0, 64)
	t.pkTree.scanRangeStop(nil, nil, func(key int64, value []byte) bool {
		if limit >= 0 && len(keys) >= limit {
			return false
		}
		e := decodeExpireAt(value)
		if e > 0 && now > e {
			return true
		}
		if where != nil {
			row, _, _ := decodeRow(meta, value)
			if !truthy(where.eval(row)) {
				return true
			}
		}
		keys = append(keys, key)
		return true
	})
	for _, key := range keys {
		value, ok := t.pkTree.Get(key)
		if !ok {
			continue // 回调里改动过（例如同一行被触发器/并发删掉）
		}
		row, _, exp := decodeRow(meta, value)
		if err := fn(key, row, exp); err != nil {
			return err
		}
	}
	return nil
}

// parseUpdate 解析并执行 UPDATE t SET col=expr[, ...] [WHERE ...] [LIMIT n]。
func (db *DB) parseUpdate(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // UPDATE
	// 低优先级修饰符
	for p.peek().is("LOW_PRIORITY") || p.peek().is("IGNORE") {
		p.next()
	}
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTableIn(tableName, p.curSchema(db))
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	if !p.matchKeyword("SET") {
		err = fmt.Errorf("expected SET")
		return
	}
	type setAssign struct {
		col string
		e   *expr
	}
	var sets []setAssign
	for {
		f, e := p.parseIdent()
		if e != nil {
			err = e
			return
		}
		if op := p.next(); op.kind != kindSym || op.val != "=" {
			err = fmt.Errorf("expected =")
			return
		}
		ae, e := p.parseExpr()
		if e != nil {
			err = e
			return
		}
		sets = append(sets, setAssign{col: f, e: ae})
		if !p.matchSym(",") {
			break
		}
	}
	var where *expr
	if p.matchKeyword("WHERE") {
		we, e := p.parseExpr()
		if e != nil {
			err = e
			return
		}
		where = we
	}
	limit := -1
	if p.matchKeyword("LIMIT") {
		v, e := p.parseValue()
		if e != nil {
			err = e
			return
		}
		if n, ok := toIntCell(v); ok && n >= 0 {
			limit = int(n) // LIMIT 0 合法：一行都不改
		}
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	// 校验 SET 列存在
	meta := t.Meta()
	for _, s := range sets {
		if meta.fieldIndexOf(s.col) < 0 {
			err = fmt.Errorf("unknown column %q in SET", s.col)
			return
		}
	}
	applyOne := func(pk int64, row map[string]interface{}, exp int64) error {
		// 保存原值：主键迁移失败时要能把旧行放回去（自动提交下没有事务兜底）
		orig := make(map[string]interface{}, len(row))
		for k, v := range row {
			orig[k] = v
		}
		for _, s := range sets {
			row[s.col] = s.e.eval(row)
		}
		// 主键被改写：旧键删除 + 新键插入，保证索引与行内 PK 一致
		// （旧实现直接把新 PK 写进行字节，树键不变，行变成"查不到"的孤儿）
		if newVal, ok := row[meta.PK]; ok {
			newPK, isInt := toIntCell(newVal)
			if !isInt {
				// 主键列只支持整数：静默 coerce 成 0 会造成索引与行内容不一致
				return fmt.Errorf("primary key must be integer")
			}
			if newPK != pk {
				if _, exists := t.pkTree.Get(newPK); exists {
					return fmt.Errorf("duplicate entry '%d' for key 'PRIMARY'", newPK)
				}
				if e := db.txnApply(p.txnID, t, pk, func() error {
					return t.Delete(pk, p.txnID, false)
				}); e != nil {
					return e
				}
				if e := db.txnApply(p.txnID, t, newPK, func() error {
					return t.Insert(newPK, row, exp, p.txnID, false)
				}); e != nil {
					// 补偿：插入失败时把旧行写回去，避免"删了没插上"丢数据
					_ = t.Insert(pk, orig, exp, p.txnID, false)
					return e
				}
				return nil
			}
		}
		return db.txnApply(p.txnID, t, pk, func() error {
			return t.Update(pk, row, exp, p.txnID, false)
		})
	}
	// 主键等值条件：点查即可
	if pk, ok := pointPK(where, meta.PK); ok && limit != 0 {
		old, se := t.Select(map[string]interface{}{meta.PK: pk}, nil, nil)
		if se != nil {
			err = se
			return
		}
		if len(old) == 0 {
			rawMsg = "0 rows updated"
			return
		}
		if e := applyOne(pk, old[0], 0); e != nil {
			err = e
			return
		}
		affected = 1
		rawMsg = "1 row updated"
		return
	}
	if limit == 0 {
		rawMsg = "0 rows updated"
		return
	}
	if e := t.scanTargets(where, limit, func(pk int64, row map[string]interface{}, exp int64) error {
		if e := applyOne(pk, row, exp); e != nil {
			return e
		}
		affected++
		return nil
	}); e != nil {
		err = e
		return
	}
	if affected == 1 {
		rawMsg = "1 row updated"
	} else {
		rawMsg = fmt.Sprintf("%d rows updated", affected)
	}
	return
}

// parseDelete 解析并执行 DELETE FROM t [WHERE ...] [ORDER BY ...] [LIMIT n]。
func (db *DB) parseDelete(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // DELETE
	for p.peek().is("LOW_PRIORITY") || p.peek().is("QUICK") || p.peek().is("IGNORE") {
		p.next()
	}
	if !p.matchKeyword("FROM") {
		err = fmt.Errorf("expected FROM")
		return
	}
	tableName, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	tableName = db.qualifyTableIn(tableName, p.curSchema(db))
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	// 别名（DELETE FROM t AS x）与 USING 形式不支持：消费掉别名即可
	if p.matchKeyword("AS") {
		p.next()
	} else if tk := p.peek(); tk.kind == kindIdent && !tk.is("WHERE") && !tk.is("ORDER") && !tk.is("LIMIT") {
		p.next()
	}
	var where *expr
	if p.matchKeyword("WHERE") {
		we, e := p.parseExpr()
		if e != nil {
			err = e
			return
		}
		where = we
	}
	// ORDER BY 对删除集合无意义（全量扫描序即主键序），跳过
	if p.matchKeyword("ORDER") {
		p.matchKeyword("BY")
		for {
			if _, e := p.parseExpr(); e != nil {
				err = e
				return
			}
			p.matchKeyword("ASC")
			p.matchKeyword("DESC")
			if !p.matchSym(",") {
				break
			}
		}
	}
	limit := -1
	if p.matchKeyword("LIMIT") {
		v, e := p.parseValue()
		if e != nil {
			err = e
			return
		}
		if n, ok := toIntCell(v); ok && n >= 0 {
			limit = int(n) // LIMIT 0 合法：一行都不删
		}
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	meta := t.Meta()
	if pk, ok := pointPK(where, meta.PK); ok && limit != 0 {
		if e := db.txnApply(p.txnID, t, pk, func() error {
			return t.Delete(pk, p.txnID, false)
		}); e != nil {
			err = e
			return
		}
		affected = 1
		rawMsg = "1 row deleted"
		return
	}
	if limit == 0 {
		rawMsg = "0 rows deleted"
		return
	}
	if e := t.scanTargets(where, limit, func(pk int64, row map[string]interface{}, exp int64) error {
		if e := db.txnApply(p.txnID, t, pk, func() error {
			return t.Delete(pk, p.txnID, false)
		}); e != nil {
			return e
		}
		affected++
		return nil
	}); e != nil {
		err = e
		return
	}
	if affected == 1 {
		rawMsg = "1 row deleted"
	} else {
		rawMsg = fmt.Sprintf("%d rows deleted", affected)
	}
	return
}

// pointPK 判断条件是否是 `pk = 整数常量`，返回该主键值。
func pointPK(where *expr, pkName string) (int64, bool) {
	if where == nil || where.kind != exFunc || where.fn != "=" || len(where.args) != 2 {
		return 0, false
	}
	for i := 0; i < 2; i++ {
		col, rhs := where.args[i], where.args[1-i]
		if col.kind == exCol && asciiEq(col.col, pkName) && col.table == "" && rhs.kind == exConst {
			if n, ok := toIntCell(rhs.val); ok {
				return n, true
			}
		}
	}
	return 0, false
}
