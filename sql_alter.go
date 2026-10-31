package main

// ==================== ALTER TABLE 扩展 ====================
// 在既有"追加列 / 表重命名"之外补齐：
//   DROP COLUMN / MODIFY [COLUMN] / CHANGE [COLUMN] / RENAME COLUMN /
//   ADD INDEX|KEY / DROP INDEX|KEY
// 所有结构变更都走写时复制（cloneMeta + setMeta），避免并发读者撕裂 Fields 切片；
// 涉及列类型/列数变化的操作走 wal_alter.go 的「单条记录整表替换」：
// 中间状态不会落盘，崩溃后要么是旧表、要么是新表，不存在半新半旧。
import (
	"fmt"
)

// alterRewriteTable 重建表结构：委托给 wal_alter.go 的单条记录整表替换。
func (db *DB) alterRewriteTable(name string, newMeta *TableMeta, transform func(old *TableMeta, row map[string]interface{}) map[string]interface{}) error {
	return db.alterTableAtomic(name, newMeta, transform)
}

// alterTableWithRows 在持锁状态下改写表结构与全部行。
// fn 返回 nil 表示该行删除（DROP COLUMN 之外的场景不会用到）。
func (db *DB) alterTableWithRows(name string, rewriteRows bool, mutate func(meta *TableMeta) error, rewrite func(t *Table, meta *TableMeta) error) error {
	db.mu.Lock()
	meta, ok := db.catalog[name]
	if !ok {
		db.mu.Unlock()
		return fmt.Errorf("table not found: %s", name)
	}
	newMeta := cloneMeta(meta, meta.Name)
	if err := mutate(newMeta); err != nil {
		db.mu.Unlock()
		return err
	}
	if err := db.writeCatalog(newMeta); err != nil {
		db.mu.Unlock()
		return err
	}
	db.catalog[name] = newMeta
	var t *Table
	if v, ok := db.tables.Load(name); ok {
		t = v.(*Table)
		t.setMeta(newMeta)
	}
	db.mu.Unlock()
	if rewriteRows && t != nil {
		if err := rewrite(t, newMeta); err != nil {
			return err
		}
	}
	return nil
}

// AlterTableDropColumn 删除一列：行是位置编码，必须重建整表。
func (db *DB) AlterTableDropColumn(name, col string) error {
	t := db.getTable(name)
	if t == nil {
		return fmt.Errorf("table not found: %s", name)
	}
	if t.Meta().PK == col {
		return fmt.Errorf("cannot drop primary key column %s", col)
	}
	if t.Meta().fieldIndexOf(col) < 0 {
		return fmt.Errorf("column %s not found in table %s", col, name)
	}
	newMeta := cloneMeta(t.Meta(), name)
	idx := newMeta.fieldIndexOf(col)
	newMeta.Fields = append(newMeta.Fields[:idx], newMeta.Fields[idx+1:]...)
	return db.alterRewriteTable(name, newMeta, func(old *TableMeta, row map[string]interface{}) map[string]interface{} {
		delete(row, col)
		return row
	})
}

// AlterTableModifyColumn 修改列类型/长度。
// 类型变化会改变行字节布局，必须重建整表；仅改长度（VARCHAR(32)->VARCHAR(64)）
// 不改变编码，直接更新 meta 即可。
func (db *DB) AlterTableModifyColumn(name, col string, ftype FieldType, flen int) error {
	t := db.getTable(name)
	if t == nil {
		return fmt.Errorf("table not found: %s", name)
	}
	idx := t.Meta().fieldIndexOf(col)
	if idx < 0 {
		return fmt.Errorf("column %s not found in table %s", col, name)
	}
	if t.Meta().Fields[idx].Type == ftype {
		// 仅长度/属性变化：位置编码不变
		return db.alterTableWithRows(name, false,
			func(meta *TableMeta) error {
				for i, f := range meta.Fields {
					if f.Name == col {
						meta.Fields[i].Len = flen
						meta.Fields[i].Type = ftype
						return nil
					}
				}
				return fmt.Errorf("column %s not found in table %s", col, name)
			}, nil)
	}
	newMeta := cloneMeta(t.Meta(), name)
	newMeta.Fields[idx].Type = ftype
	newMeta.Fields[idx].Len = flen
	// 类型转换在重建时逐行进行（int -> varchar / varchar -> int）
	return db.alterRewriteTable(name, newMeta, func(old *TableMeta, row map[string]interface{}) map[string]interface{} {
		v := row[col]
		switch ftype {
		case TypeInt:
			row[col] = coerceInt(v)
		case TypeBool:
			row[col] = truthy(v)
		case TypeDate, TypeDatetime:
			row[col] = coerceTime(v, ftype == TypeDate)
		case TypeDecimal:
			row[col] = coerceDecimal(v, flen)
		case TypeText, TypeBlob:
			if v != nil {
				row[col] = valueString(v)
			}
		case TypeVarchar:
			if v != nil {
				if s, ok := v.(string); ok {
					row[col] = s
				} else {
					row[col] = fmt.Sprint(v)
				}
			}
		}
		return row
	})
}

// AlterTableRenameColumn 列重命名：字段顺序不变，无需重写行。
func (db *DB) AlterTableRenameColumn(name, oldCol, newCol string) error {
	return db.alterTableWithRows(name, false,
		func(meta *TableMeta) error {
			for i, f := range meta.Fields {
				if f.Name == oldCol {
					meta.Fields[i].Name = newCol
					if meta.PK == oldCol {
						meta.PK = newCol
					}
					return nil
				}
			}
			return fmt.Errorf("column %s not found in table %s", oldCol, name)
		}, nil)
}

// AlterTableAddIndex 添加二级索引（回填现有行）。
func (db *DB) AlterTableAddIndex(name, idxName, col string) error {
	return db.alterTableWithRows(name, false,
		func(meta *TableMeta) error {
			if _, exists := meta.Indexes[idxName]; exists {
				return fmt.Errorf("index %s already exists", idxName)
			}
			if meta.fieldIndexOf(col) < 0 {
				return fmt.Errorf("column %s not found in table %s", col, name)
			}
			if meta.Indexes == nil {
				meta.Indexes = map[string]string{}
			}
			meta.Indexes[idxName] = col
			return nil
		},
		func(t *Table, meta *TableMeta) error {
			t.rebuildIndexes()
			return nil
		})
}

// AlterTableDropIndex 删除二级索引。
func (db *DB) AlterTableDropIndex(name, idxName string) error {
	return db.alterTableWithRows(name, false,
		func(meta *TableMeta) error {
			if _, ok := meta.Indexes[idxName]; !ok {
				return fmt.Errorf("index %s not found in table %s", idxName, name)
			}
			delete(meta.Indexes, idxName)
			return nil
		},
		func(t *Table, meta *TableMeta) error {
			t.rebuildIndexes()
			return nil
		})
}

// rewriteAllRows 用新 meta 重写全部行（逐行 Update，写 WAL）。
func (db *DB) rewriteAllRows(t *Table, meta *TableMeta) error {
	type kv struct {
		pk  int64
		val []byte
	}
	var rows []kv
	t.pkTree.scanRangeStop(nil, nil, func(k int64, v []byte) bool {
		rows = append(rows, kv{pk: k, val: v})
		return true
	})
	for _, r := range rows {
		row, _, expireAt := decodeRow(meta, r.val)
		if err := t.Update(r.pk, row, expireAt, 0, false); err != nil {
			return err
		}
	}
	return db.flushWAL()
}
