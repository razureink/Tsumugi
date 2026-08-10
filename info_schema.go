package main

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ==================== information_schema 虚拟系统库 ====================
// phpMyAdmin 等客户端通过 information_schema.SCHEMATA / TABLES / COLUMNS /
// STATISTICS 等查询库表结构。本文件用内存 catalog 构造这些虚拟表，
// 使兼容客户端能正常连接、列库、浏览表。

// infoSchemaCols 定义各虚拟表应对外暴露的列（小写）。
var infoSchemaCols = map[string][]string{
	"SCHEMATA": {
		"CATALOG_NAME", "SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME",
		"DEFAULT_COLLATION_NAME", "SQL_PATH", "DEFAULT_ENCRYPTION",
	},
	"TABLES": {
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE", "ENGINE",
		"VERSION", "ROW_FORMAT", "TABLE_ROWS", "AVG_ROW_LENGTH", "DATA_LENGTH",
		"MAX_DATA_LENGTH", "INDEX_LENGTH", "DATA_FREE", "AUTO_INCREMENT",
		"CREATE_TIME", "UPDATE_TIME", "CHECK_TIME", "TABLE_COLLATION", "CHECKSUM",
		"CREATE_OPTIONS", "TABLE_COMMENT",
	},
	"COLUMNS": {
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "CHARACTER_OCTET_LENGTH", "NUMERIC_PRECISION",
		"NUMERIC_SCALE", "DATETIME_PRECISION", "CHARACTER_SET_NAME",
		"COLLATION_NAME", "COLUMN_TYPE", "COLUMN_KEY", "EXTRA", "PRIVILEGES",
		"COLUMN_COMMENT", "GENERATION_EXPRESSION",
	},
	"STATISTICS": {
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "NON_UNIQUE",
		"INDEX_SCHEMA", "INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME",
		"COLLATION", "CARDINALITY", "SUB_PART", "PACKED", "NULLABLE",
		"INDEX_TYPE", "COMMENT", "INDEX_COMMENT", "IS_VISIBLE",
	},
	"TABLE_CONSTRAINTS": {
		"CONSTRAINT_CATALOG", "CONSTRAINT_SCHEMA", "CONSTRAINT_NAME",
		"TABLE_SCHEMA", "TABLE_NAME", "CONSTRAINT_TYPE",
	},
	"KEY_COLUMN_USAGE": {
		"CONSTRAINT_CATALOG", "CONSTRAINT_SCHEMA", "CONSTRAINT_NAME",
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "POSITION_IN_UNIQUE_CONSTRAINT",
		"REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME",
	},
	"USER_PRIVILEGES": {
		"GRANTEE", "TABLE_CATALOG", "PRIVILEGE_TYPE", "IS_GRANTABLE",
	},
	"SCHEMA_PRIVILEGES": {
		"GRANTEE", "TABLE_CATALOG", "TABLE_SCHEMA", "PRIVILEGE_TYPE", "IS_GRANTABLE",
	},
	"TABLE_PRIVILEGES": {
		"GRANTEE", "TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "PRIVILEGE_TYPE", "IS_GRANTABLE",
	},
	"ROUTINES": {
		"SPECIFIC_NAME", "ROUTINE_SCHEMA", "ROUTINE_NAME", "ROUTINE_TYPE",
		"DATA_TYPE", "ROUTINE_BODY", "ROUTINE_DEFINITION", "SQL_MODE",
	},
	"TRIGGERS": {
		"TRIGGER_CATALOG", "TRIGGER_SCHEMA", "TRIGGER_NAME", "EVENT_MANIPULATION",
		"EVENT_OBJECT_SCHEMA", "EVENT_OBJECT_TABLE", "ACTION_ORIENTATION",
		"ACTION_TIMING", "ACTION_STATEMENT", "ACTION_CONDITION", "ACTION_BODY",
	},
	"VIEWS": {
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "VIEW_DEFINITION",
		"CHECK_OPTION", "IS_UPDATABLE", "DEFINER", "SECURITY_TYPE",
	},
	"PARTITIONS": {
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "PARTITION_NAME",
		"SUBPARTITION_NAME", "PARTITION_ORDINAL_POSITION", "PARTITION_METHOD",
		"SUBPARTITION_METHOD", "PARTITION_EXPRESSION",
	},
	"CHARACTER_SETS": {
		"CHARACTER_SET_NAME", "DEFAULT_COLLATE_NAME", "DESCRIPTION", "MAXLEN",
	},
	"COLLATIONS": {
		"COLLATION_NAME", "CHARACTER_SET_NAME", "ID", "IS_DEFAULT",
		"IS_COMPILED", "SORTLEN",
	},
	"ENGINES": {
		"ENGINE", "SUPPORT", "COMMENT", "TRANSACTIONS", "XA", "SAVEPOINTS",
	},
	"PLUGINS": {
		"PLUGIN_NAME", "PLUGIN_VERSION", "PLUGIN_STATUS", "PLUGIN_TYPE",
		"PLUGIN_LIBRARY", "PLUGIN_LICENSE",
	},
	"PARAMETERS": {
		"SPECIFIC_CATALOG", "SPECIFIC_SCHEMA", "SPECIFIC_NAME", "ORDINAL_POSITION",
		"PARAMETER_MODE", "PARAMETER_NAME", "DATA_TYPE", "PARAMETER_DEFAULT",
	},
	"PROCESSLIST": {
		"ID", "USER", "HOST", "DB", "COMMAND", "TIME", "STATE", "INFO",
	},
	"GLOBAL_STATUS":  {"VARIABLE_NAME", "VARIABLE_VALUE"},
	"SESSION_STATUS": {"VARIABLE_NAME", "VARIABLE_VALUE"},
}

// emptyInfoTables 这些虚拟表始终无行（但需正确列头以兼容客户端）。
var emptyInfoTables = map[string]bool{
	"ROUTINES": true, "TRIGGERS": true, "VIEWS": true, "PARTITIONS": true,
	"PARAMETERS": true, "PROCESSLIST": true,
}

// queryInfoSchema 处理 SELECT ... FROM information_schema.<table>。
func (db *DB) queryInfoSchema(name string, conds map[string]interface{}, selectCols []string, colSrc map[string]string) (columns []string, rows [][]interface{}, err error) {
	upper := strings.ToUpper(name)
	allCols := infoSchemaCols[upper]
	if allCols == nil {
		err = fmt.Errorf("table 'information_schema.%s' doesn't exist", upper)
		return
	}
	recs := db.infoSchemaRecords(upper)
	// 过滤条件（等值，大小写不敏感匹配字符串）
	var out []map[string]interface{}
	for _, rec := range recs {
		ok := true
		for k, v := range conds {
			want := fmt.Sprintf("%v", v)
			got := fmt.Sprintf("%v", rec[strings.ToUpper(k)])
			if !strings.EqualFold(got, want) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, rec)
		}
	}
	columns, rows = projectInfoRows(allCols, out, selectCols, colSrc)
	return
}

// projectInfoRows 按 selectCols 投影（nil=全部列）。colSrc 提供 输出列名->源列名 别名映射。
func projectInfoRows(allCols []string, recs []map[string]interface{}, selectCols []string, colSrc map[string]string) ([]string, [][]interface{}) {
	if selectCols == nil {
		cols := make([]string, len(allCols))
		for i, c := range allCols {
			cols[i] = strings.ToLower(c)
		}
		rows := make([][]interface{}, 0, len(recs))
		for _, rec := range recs {
			row := make([]interface{}, 0, len(allCols))
			for _, c := range allCols {
				row = append(row, rec[c])
			}
			rows = append(rows, row)
		}
		return cols, rows
	}
	idx := make([]int, 0, len(selectCols))
	cols := make([]string, 0, len(selectCols))
	for _, sc := range selectCols {
		src := sc
		if colSrc != nil {
			if s, ok := colSrc[strings.ToLower(sc)]; ok {
				src = s
			}
		}
		for i, c := range allCols {
			if strings.EqualFold(src, c) {
				idx = append(idx, i)
				cols = append(cols, strings.ToLower(sc))
				break
			}
		}
	}
	rows := make([][]interface{}, 0, len(recs))
	for _, rec := range recs {
		row := make([]interface{}, 0, len(idx))
		for _, i := range idx {
			row = append(row, rec[allCols[i]])
		}
		rows = append(rows, row)
	}
	return cols, rows
}

// infoSchemaRecords 构造指定虚拟表的记录。
func (db *DB) infoSchemaRecords(name string) []map[string]interface{} {
	switch name {
	case "SCHEMATA":
		var out []map[string]interface{}
		for _, d := range mysqlCfg.get().Databases {
			out = append(out, map[string]interface{}{
				"CATALOG_NAME": "def", "SCHEMA_NAME": d,
				"DEFAULT_CHARACTER_SET_NAME": "utf8mb4",
				"DEFAULT_COLLATION_NAME":     "utf8mb4_unicode_ci",
				"SQL_PATH":                   nil, "DEFAULT_ENCRYPTION": "NO",
			})
		}
		return out
	case "TABLES":
		var out []map[string]interface{}
		db.mu.RLock()
		for name := range db.catalog {
			schema, table := splitQualifiedTable(name)
			var rowCount int64
			if t := db.getTable(name); t != nil {
				rowCount = t.pkTree.Size()
			}
			out = append(out, map[string]interface{}{
				"TABLE_CATALOG": "def", "TABLE_SCHEMA": schema, "TABLE_NAME": table,
				"TABLE_TYPE": "BASE TABLE", "ENGINE": "InnoDB", "VERSION": "10",
				"ROW_FORMAT": "Dynamic", "TABLE_ROWS": rowCount, "AVG_ROW_LENGTH": "0",
				"DATA_LENGTH": "0", "MAX_DATA_LENGTH": "0", "INDEX_LENGTH": "0",
				"DATA_FREE": "0", "AUTO_INCREMENT": "1", "CREATE_TIME": "",
				"UPDATE_TIME": "", "CHECK_TIME": "", "TABLE_COLLATION": "utf8mb4_unicode_ci",
				"CHECKSUM": nil, "CREATE_OPTIONS": "", "TABLE_COMMENT": "",
			})
		}
		db.mu.RUnlock()
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a["TABLE_SCHEMA"] != b["TABLE_SCHEMA"] {
				return a["TABLE_SCHEMA"].(string) < b["TABLE_SCHEMA"].(string)
			}
			return a["TABLE_NAME"].(string) < b["TABLE_NAME"].(string)
		})
		return out
	case "COLUMNS":
		var out []map[string]interface{}
		db.mu.RLock()
		for name, meta := range db.catalog {
			schema, table := splitQualifiedTable(name)
			for i, f := range meta.Fields {
				colType := fieldTypeName(f.Type)
				key := ""
				if meta.PK == f.Name {
					key = "PRI"
				}
				out = append(out, map[string]interface{}{
					"TABLE_CATALOG": "def", "TABLE_SCHEMA": schema, "TABLE_NAME": table,
					"COLUMN_NAME": f.Name, "ORDINAL_POSITION": i + 1,
					"COLUMN_DEFAULT": nil, "IS_NULLABLE": "YES", "DATA_TYPE": colType,
					"CHARACTER_MAXIMUM_LENGTH": nil, "CHARACTER_OCTET_LENGTH": nil,
					"NUMERIC_PRECISION": nil, "NUMERIC_SCALE": nil, "DATETIME_PRECISION": nil,
					"CHARACTER_SET_NAME": "utf8mb4", "COLLATION_NAME": "utf8mb4_unicode_ci",
					"COLUMN_TYPE": colType, "COLUMN_KEY": key, "EXTRA": "", "PRIVILEGES": "",
					"COLUMN_COMMENT": "", "GENERATION_EXPRESSION": "",
				})
			}
		}
		db.mu.RUnlock()
		return out
	case "STATISTICS":
		var out []map[string]interface{}
		db.mu.RLock()
		for name, meta := range db.catalog {
			schema, table := splitQualifiedTable(name)
			seq := 0
			if meta.PK != "" {
				seq++
				out = append(out, map[string]interface{}{
					"TABLE_CATALOG": "def", "TABLE_SCHEMA": schema, "TABLE_NAME": table,
					"NON_UNIQUE": "0", "INDEX_SCHEMA": schema, "INDEX_NAME": "PRIMARY",
					"SEQ_IN_INDEX": seq, "COLUMN_NAME": meta.PK, "COLLATION": "A",
					"CARDINALITY": "0", "SUB_PART": nil, "PACKED": nil, "NULLABLE": "",
					"INDEX_TYPE": "BTREE", "COMMENT": "", "INDEX_COMMENT": "", "IS_VISIBLE": "YES",
				})
			}
			for ixName, field := range meta.Indexes {
				seq++
				out = append(out, map[string]interface{}{
					"TABLE_CATALOG": "def", "TABLE_SCHEMA": schema, "TABLE_NAME": table,
					"NON_UNIQUE": "1", "INDEX_SCHEMA": schema, "INDEX_NAME": ixName,
					"SEQ_IN_INDEX": seq, "COLUMN_NAME": field, "COLLATION": "A",
					"CARDINALITY": "0", "SUB_PART": nil, "PACKED": nil, "NULLABLE": "",
					"INDEX_TYPE": "BTREE", "COMMENT": "", "INDEX_COMMENT": "", "IS_VISIBLE": "YES",
				})
			}
		}
		db.mu.RUnlock()
		return out
	case "TABLE_CONSTRAINTS":
		var out []map[string]interface{}
		db.mu.RLock()
		for name, meta := range db.catalog {
			schema, table := splitQualifiedTable(name)
			if meta.PK != "" {
				out = append(out, map[string]interface{}{
					"CONSTRAINT_CATALOG": "def", "CONSTRAINT_SCHEMA": schema,
					"CONSTRAINT_NAME": "PRIMARY", "TABLE_SCHEMA": schema, "TABLE_NAME": table,
					"CONSTRAINT_TYPE": "PRIMARY KEY",
				})
			}
			for ixName := range meta.Indexes {
				out = append(out, map[string]interface{}{
					"CONSTRAINT_CATALOG": "def", "CONSTRAINT_SCHEMA": schema,
					"CONSTRAINT_NAME": ixName, "TABLE_SCHEMA": schema, "TABLE_NAME": table,
					"CONSTRAINT_TYPE": "INDEX",
				})
			}
		}
		db.mu.RUnlock()
		return out
	case "KEY_COLUMN_USAGE":
		var out []map[string]interface{}
		db.mu.RLock()
		for name, meta := range db.catalog {
			schema, table := splitQualifiedTable(name)
			if meta.PK != "" {
				out = append(out, map[string]interface{}{
					"CONSTRAINT_CATALOG": "def", "CONSTRAINT_SCHEMA": schema,
					"CONSTRAINT_NAME": "PRIMARY", "TABLE_CATALOG": "def",
					"TABLE_SCHEMA": schema, "TABLE_NAME": table, "COLUMN_NAME": meta.PK,
					"ORDINAL_POSITION": "1", "POSITION_IN_UNIQUE_CONSTRAINT": nil,
					"REFERENCED_TABLE_SCHEMA": nil, "REFERENCED_TABLE_NAME": nil,
					"REFERENCED_COLUMN_NAME": nil,
				})
			}
			for ixName, field := range meta.Indexes {
				out = append(out, map[string]interface{}{
					"CONSTRAINT_CATALOG": "def", "CONSTRAINT_SCHEMA": schema,
					"CONSTRAINT_NAME": ixName, "TABLE_CATALOG": "def",
					"TABLE_SCHEMA": schema, "TABLE_NAME": table, "COLUMN_NAME": field,
					"ORDINAL_POSITION": "1", "POSITION_IN_UNIQUE_CONSTRAINT": nil,
					"REFERENCED_TABLE_SCHEMA": nil, "REFERENCED_TABLE_NAME": nil,
					"REFERENCED_COLUMN_NAME": nil,
				})
			}
		}
		db.mu.RUnlock()
		return out
	case "USER_PRIVILEGES", "SCHEMA_PRIVILEGES", "TABLE_PRIVILEGES":
		var out []map[string]interface{}
		for _, u := range mysqlCfg.get().Users {
			privs := "SELECT,INSERT,UPDATE,DELETE,CREATE,DROP,INDEX,ALTER"
			rec := map[string]interface{}{
				"GRANTEE": "'" + u.User + "'@'" + u.Host + "'", "TABLE_CATALOG": "def",
				"PRIVILEGE_TYPE": privs, "IS_GRANTABLE": "NO",
			}
			if name == "SCHEMA_PRIVILEGES" {
				rec["TABLE_SCHEMA"] = "tsumugi"
				out = append(out, rec)
			} else if name == "TABLE_PRIVILEGES" {
				rec["TABLE_SCHEMA"] = "tsumugi"
				rec["TABLE_NAME"] = "%"
				out = append(out, rec)
			} else {
				out = append(out, rec)
			}
		}
		return out
	case "CHARACTER_SETS":
		return []map[string]interface{}{
			{"CHARACTER_SET_NAME": "utf8mb4", "DEFAULT_COLLATE_NAME": "utf8mb4_unicode_ci", "DESCRIPTION": "UTF-8 Unicode", "MAXLEN": "4"},
			{"CHARACTER_SET_NAME": "utf8", "DEFAULT_COLLATE_NAME": "utf8_general_ci", "DESCRIPTION": "UTF-8 Unicode", "MAXLEN": "3"},
			{"CHARACTER_SET_NAME": "latin1", "DEFAULT_COLLATE_NAME": "latin1_swedish_ci", "DESCRIPTION": "cp1252 West European", "MAXLEN": "1"},
			{"CHARACTER_SET_NAME": "ascii", "DEFAULT_COLLATE_NAME": "ascii_general_ci", "DESCRIPTION": "US ASCII", "MAXLEN": "1"},
		}
	case "COLLATIONS":
		var out []map[string]interface{}
		for i, c := range []string{"utf8mb4_unicode_ci", "utf8mb4_general_ci", "utf8_general_ci", "latin1_swedish_ci", "ascii_general_ci"} {
			charset := strings.SplitN(c, "_", 2)[0]
			def := ""
			if c == "utf8mb4_unicode_ci" {
				def = "Yes"
			}
			out = append(out, map[string]interface{}{
				"COLLATION_NAME": c, "CHARACTER_SET_NAME": charset, "ID": 45 + i,
				"IS_DEFAULT": def, "IS_COMPILED": "Yes", "SORTLEN": "8",
			})
		}
		return out
	case "ENGINES":
		return []map[string]interface{}{
			{"ENGINE": "InnoDB", "SUPPORT": "DEFAULT", "COMMENT": "Supports transactions, row-level locking, and foreign keys", "TRANSACTIONS": "YES", "XA": "YES", "SAVEPOINTS": "YES"},
			{"ENGINE": "MEMORY", "SUPPORT": "YES", "COMMENT": "Hash based, stored in memory, useful for temporary tables", "TRANSACTIONS": "NO", "XA": "NO", "SAVEPOINTS": "NO"},
			{"ENGINE": "MyISAM", "SUPPORT": "YES", "COMMENT": "MyISAM storage engine", "TRANSACTIONS": "NO", "XA": "NO", "SAVEPOINTS": "NO"},
		}
	case "PLUGINS":
		var out []map[string]interface{}
		for _, p := range []string{"InnoDB", "mysql_native_password", "sha256_password", "caching_sha2_password"} {
			out = append(out, map[string]interface{}{
				"PLUGIN_NAME": p, "PLUGIN_VERSION": "1.0", "PLUGIN_STATUS": "ACTIVE",
				"PLUGIN_TYPE": "STORAGE ENGINE", "PLUGIN_LIBRARY": nil, "PLUGIN_LICENSE": "GPL",
			})
		}
		return out
	case "GLOBAL_STATUS", "SESSION_STATUS":
		var out []map[string]interface{}
		snap := db.stats.Snapshot()
		for _, k := range []string{"total_commands", "total_errors"} {
			out = append(out, map[string]interface{}{
				"VARIABLE_NAME": strings.ToUpper(k), "VARIABLE_VALUE": fmt.Sprintf("%v", snap[k]),
			})
		}
		return out
	}
	if emptyInfoTables[name] {
		recs := make([]map[string]interface{}, 0)
		return recs
	}
	return nil
}

// splitQualifiedTable 拆 "schema.table" 物理表名为 (schema, table)；无前缀视为 tsumugi 库。
func splitQualifiedTable(name string) (string, string) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "tsumugi", name
}

// infoSchemaTableExists 判断是否存在的 information_schema 虚拟表。
func infoSchemaTableExists(name string) bool {
	_, ok := infoSchemaCols[strings.ToUpper(name)]
	return ok
}

// qualifyForDB 解析 "表名" 为当前库内的物理表名（用于 SHOW COLUMNS 等无逗号场景）。
func (db *DB) qualifyForDB(dbName, table string) string {
	if strings.Contains(table, ".") {
		return table
	}
	if dbName == "" || dbName == "tsumugi" {
		return table
	}
	return dbName + "." + table
}

// describeTable 实现 SHOW [FULL] COLUMNS FROM tbl [FROM db]。
func (db *DB) describeTable(dbName, table string) (columns []string, rows [][]interface{}, err error) {
	t := db.getTable(db.qualifyForDB(dbName, table))
	if t == nil {
		err = fmt.Errorf("table not found: %s", table)
		return
	}
	columns = []string{"Field", "Type", "Null", "Key", "Default", "Extra"}
	for _, f := range t.meta.Fields {
		key := ""
		if t.meta.PK == f.Name {
			key = "PRI"
		}
		rows = append(rows, []interface{}{
			f.Name,
			fieldTypeName(f.Type),
			"YES",
			key,
			nil,
			"",
		})
	}
	return
}

// tableStatus 实现 SHOW TABLE STATUS FROM db。
func (db *DB) tableStatus(dbName string) (columns []string, rows [][]interface{}, err error) {
	tables, e := db.adminTables(dbName)
	if e != nil {
		err = e
		return
	}
	columns = []string{
		"Name", "Engine", "Version", "Row_format", "Rows", "Avg_row_length",
		"Data_length", "Max_data_length", "Index_length", "Data_free",
		"Auto_increment", "Create_time", "Update_time", "Check_time",
		"Collation", "Checksum", "Create_options", "Comment",
	}
	for _, tb := range tables {
		show := tb["name"].(string)
		if dbName == "tsumugi" {
			if i := strings.IndexByte(show, '.'); i >= 0 {
				show = show[i+1:]
			}
		}
		rows = append(rows, []interface{}{
			show, "InnoDB", "10", "Dynamic", tb["row_count"], "0",
			"0", "0", "0", "0", "1", "", "", "",
			"utf8mb4_unicode_ci", nil, "", "",
		})
	}
	return
}

// showIndex 实现 SHOW INDEX FROM tbl [FROM db]。
func (db *DB) showIndex(dbName, table string) (columns []string, rows [][]interface{}, err error) {
	t := db.getTable(db.qualifyForDB(dbName, table))
	if t == nil {
		err = fmt.Errorf("table not found: %s", table)
		return
	}
	columns = []string{
		"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name",
		"Collation", "Cardinality", "Sub_part", "Packed", "Null", "Index_type",
		"Comment", "Index_comment", "Visible",
	}
	show := strings.TrimPrefix(t.meta.Name, dbName+".")
	if dbName == "" || dbName == "tsumugi" {
		show = strings.TrimPrefix(t.meta.Name, "tsumugi.")
	}
	if i := strings.LastIndexByte(show, '.'); i >= 0 && dbName == "" {
		show = show[i+1:]
	}
	seq := 0
	if t.meta.PK != "" {
		seq++
		rows = append(rows, []interface{}{show, "0", "PRIMARY", seq, t.meta.PK, "A", "0", nil, nil, "", "BTREE", "", "", "YES"})
	}
	for ixName, field := range t.meta.Indexes {
		seq++
		rows = append(rows, []interface{}{show, "1", ixName, seq, field, "A", "0", nil, nil, "", "BTREE", "", "", "YES"})
	}
	return
}

// createTableStmt 生成 SHOW CREATE TABLE 的建表语句文本。
func (db *DB) createTableStmt(tblName string) string {
	t := db.getTable(tblName)
	if t == nil {
		return "CREATE TABLE `" + tblName + "` ()"
	}
	var sb strings.Builder
	sb.WriteString("CREATE TABLE `" + tblName + "` (\n")
	first := true
	for _, f := range t.meta.Fields {
		if !first {
			sb.WriteString(",\n")
		}
		first = false
		sb.WriteString("  `" + f.Name + "` " + fieldTypeName(f.Type))
		if t.meta.PK == f.Name {
			sb.WriteString(" NOT NULL")
		}
	}
	if t.meta.PK != "" {
		sb.WriteString(",\n  PRIMARY KEY (`" + t.meta.PK + "`)")
	}
	for ixName, field := range t.meta.Indexes {
		sb.WriteString(",\n  KEY `" + ixName + "` (`" + field + "`)")
	}
	sb.WriteString("\n) ENGINE=InnoDB")
	return sb.String()
}

// countSelect 处理 SELECT COUNT(*) FROM tbl [WHERE ..]。
// 目前仅支持纯等值 WHERE（phpMyAdmin 惯例即如此）。
func (db *DB) countSelect(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	tableName, e := p.readTableIdent()
	if e != nil {
		err = e
		return
	}
	for p.peek().kind == "sym" && p.peek().val == "." {
		p.next()
		part, pe := p.readTableIdent()
		if pe != nil {
			err = pe
			return
		}
		tableName += "." + part
	}
	conds := map[string]interface{}{}
	if strings.HasPrefix(strings.ToLower(tableName), "information_schema.") {
		if p.matchKeyword("WHERE") {
			for {
				f, e := p.parseIdent()
				if e != nil {
					err = e
					return
				}
				op := p.next()
				if op.kind != "sym" || op.val != "=" {
					err = fmt.Errorf("unsupported condition in COUNT")
					return
				}
				v, e := p.parseValue()
				if e != nil {
					err = e
					return
				}
				conds[f] = v
				if !p.matchKeyword("AND") {
					break
				}
			}
		}
		it := strings.ToLower(strings.TrimPrefix(tableName, "information_schema."))
		_, recs, e := db.queryInfoSchema(it, conds, nil, nil)
		if e != nil {
			err = e
			return
		}
		columns = []string{"COUNT(*)"}
		rows = [][]interface{}{{int64(len(recs))}}
		return
	}
	// 物理表
	lower := strings.ToLower(tableName)
	if !strings.HasPrefix(lower, "mysql.") {
		tableName = db.qualifyTable(tableName)
	}
	t := db.getTable(tableName)
	if t == nil {
		err = fmt.Errorf("table not found: %s", tableName)
		return
	}
	if p.matchKeyword("WHERE") {
		for {
			f, e := p.parseIdent()
			if e != nil {
				err = e
				return
			}
			op := p.next()
			if op.kind != "sym" {
				err = fmt.Errorf("unsupported condition in COUNT")
				return
			}
			if op.val != "=" {
				err = fmt.Errorf("unsupported condition in COUNT")
				return
			}
			v, e := p.parseValue()
			if e != nil {
				err = e
				return
			}
			conds[f] = v
			if !p.matchKeyword("AND") {
				break
			}
		}
	}
	// 物理表计数：遍历计数而非大 limit 预分配（避免 OOM）
	var count int64
	t.mu.RLock()
	scan := func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && time.Now().UnixNano() > e {
			return true
		}
		if len(conds) > 0 {
			row, _, _ := decodeRow(t.meta, value)
			if !matchConditions(row, conds) {
				return true
			}
		}
		count++
		return true
	}
	t.pkTree.scanRangeStop(nil, nil, scan)
	t.mu.RUnlock()
	columns = []string{"COUNT(*)"}
	rows = [][]interface{}{{count}}
	return
}

// queryScalarSelect 处理无 FROM 的标量查询（@@vars / 函数），phpMyAdmin 连接期高频使用。
// 支持多列：SELECT @@session.sql_mode, @@character_set_client AS csc, VERSION(), NOW() 等。
// 仅当整条查询都是标量项时才命中；否则返回 rows=nil 交由常规 SELECT 路径处理。
func (db *DB) queryScalarSelect(p *sqlParser) (columns []string, rows [][]interface{}, err error) {
	save := p.i
	var cols []string
	var vals []interface{}
	for {
		name, val, ok := db.parseScalarExpr(p)
		if !ok {
			p.i = save
			return nil, nil, nil
		}
		cols = append(cols, name)
		vals = append(vals, val)
		// 可选的 AS 别名
		if p.matchKeyword("AS") {
			if t := p.peek(); t.kind == "ident" || t.kind == "kw" {
				cols[len(cols)-1] = t.val
				p.next()
			}
		}
		if p.peek().kind == "sym" && p.peek().val == "," {
			p.next()
			continue
		}
		break
	}
	// 尾部可选 LIMIT 1 / 分号
	p.matchKeyword("LIMIT")
	if t := p.peek(); t.kind == "num" {
		p.next()
	}
	// 必须是语句结尾（无 FROM），否则交给常规 SELECT 解析
	if t := p.peek(); t.kind != "eof" && !(t.kind == "sym" && t.val == ";") {
		p.i = save
		return nil, nil, nil
	}
	columns = cols
	rows = [][]interface{}{vals}
	return
}

// parseScalarExpr 解析单个标量表达式：@@var、函数调用、字符串或数字。
func (db *DB) parseScalarExpr(p *sqlParser) (name string, val interface{}, ok bool) {
	t := p.peek()
	// @@变量：tokenizer 将 @@ 拆为两个 @ 符号，后接 [session.|global.]ident[.ident]
	if t.kind == "sym" && t.val == "@" {
		p.next()
		if p.peek().kind == "sym" && p.peek().val == "@" {
			p.next()
		}
		var sb strings.Builder
		for {
			nt := p.peek()
			if nt.kind == "ident" || nt.kind == "kw" {
				sb.WriteString(nt.val)
				p.next()
				continue
			}
			if nt.kind == "sym" && nt.val == "." {
				sb.WriteString(".")
				p.next()
				continue
			}
			break
		}
		if sb.Len() == 0 {
			return "", nil, false
		}
		name = sb.String()
		// 列名必须带 @@ 前缀（phpMyAdmin 按 $row['@@version'] 等读取）
		colName := "@@" + name
		return colName, db.systemVar(name), true
	}
	// 函数调用：IDENT( 形式
	if t.kind == "ident" {
		fn := strings.ToUpper(t.val)
		save := p.i
		p.next()
		if p.peek().kind == "sym" && p.peek().val == "(" {
			// 跳过括号内内容（含嵌套括号/字符串），仅消费 token
			depth := 1
			p.next() // 消费 (
			for depth > 0 {
				tok := p.next()
				if tok.kind == "eof" {
					p.i = save
					return "", nil, false
				}
				if tok.kind == "sym" {
					if tok.val == "(" {
						depth++
					} else if tok.val == ")" {
						depth--
					}
				}
			}
			switch fn {
			case "VERSION":
				val = mysqlCfg.get().Version
			case "NOW", "CURRENT_TIMESTAMP", "LOCALTIME", "LOCALTIMESTAMP":
				val = time.Now().Format("2006-01-02 15:04:05")
			case "DATABASE", "SCHEMA":
				val = db.getCurDB()
			case "CURRENT_USER", "USER", "SESSION_USER", "SYSTEM_USER":
				usr := ""
				if len(mysqlCfg.get().Users) > 0 {
					usr = mysqlCfg.get().Users[0].User + "@" + mysqlCfg.get().Users[0].Host
				}
				val = usr
			case "CONNECTION_ID":
				val = int64(1)
			case "COLLATION":
				val = "utf8mb4_unicode_ci"
			case "CHARSET":
				val = "utf8mb4"
			case "FOUND_ROWS":
				val = int64(0)
			case "ROW_COUNT":
				val = int64(0)
			case "LAST_INSERT_ID":
				val = int64(0)
			case "CURRENT_DATE", "CURDATE":
				val = time.Now().Format("2006-01-02")
			case "CURTIME":
				val = time.Now().Format("15:04:05")
			case "UUID":
				val = newUUID()
			default:
				val = ""
			}
			return strings.ToLower(fn), val, true
		}
		p.i = save
		return "", nil, false
	}
	// 字符串或数字字面量
	if t.kind == "str" {
		p.next()
		return t.val, t.val, true
	}
	if t.kind == "num" {
		p.next()
		if n, e := strconv.ParseInt(t.val, 10, 64); e == nil {
			return t.val, n, true
		}
		return t.val, t.val, true
	}
	return "", nil, false
}

// systemVar 返回 @@ 系统变量值（含常用兜底，避免 phpMyAdmin 拿空值导致白屏）。
func (db *DB) systemVar(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimPrefix(lower, "session.")
	lower = strings.TrimPrefix(lower, "global.")
	if v, ok := mysqlCfg.get().Variables[lower]; ok && v != "" {
		return v
	}
	switch lower {
	case "version_comment":
		return "tsumugi"
	case "character_set_server", "character_set_database", "character_set_connection", "character_set_results", "character_set_client":
		return "utf8mb4"
	case "collation_server", "collation_database", "collation_connection":
		return "utf8mb4_unicode_ci"
	case "lower_case_table_names":
		return "0"
	case "max_allowed_packet":
		return "67108864"
	case "time_zone", "system_time_zone":
		return "SYSTEM"
	case "version":
		return mysqlCfg.get().Version
	case "have_ssl":
		return "DISABLED"
	case "sql_mode":
		return "NO_ENGINE_SUBSTITUTION"
	case "auto_increment_increment":
		return "1"
	case "init_connect", "license":
		return ""
	case "interactive_timeout", "wait_timeout":
		return "28800"
	case "net_buffer_length":
		return "16384"
	case "net_write_timeout":
		return "60"
	case "performance_schema":
		return "0"
	case "query_cache_size", "query_cache_type":
		return "0"
	case "transaction_isolation":
		return "REPEATABLE-READ"
	case "character_set_system":
		return "utf8mb3"
	}
	return ""
}

// newUUID 生成一个 v4 UUID 字符串（标准库无 UUID，直接按 RFC 4122 生成）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
