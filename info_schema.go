package main

import (
	"fmt"
	"sort"
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
func (db *DB) queryInfoSchema(name string, conds map[string]interface{}, selectCols []string) (columns []string, rows [][]interface{}, err error) {
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
	columns, rows = projectInfoRows(allCols, out, selectCols)
	return
}

// projectInfoRows 按 selectCols 投影（nil=全部列）。
func projectInfoRows(allCols []string, recs []map[string]interface{}, selectCols []string) ([]string, [][]interface{}) {
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
		for i, c := range allCols {
			if strings.EqualFold(sc, c) {
				idx = append(idx, i)
				cols = append(cols, strings.ToLower(c))
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
		_, recs, e := db.queryInfoSchema(it, conds, nil)
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
func (db *DB) queryScalarSelect(p *sqlParser) (columns []string, rows [][]interface{}, err error) {
	// SELECT @@session.sql_mode / @@version_comment / @@character_set_server 等
	if p.peek().kind == "sym" && p.peek().val == "@" {
		p.next()
		var sb strings.Builder
		sb.WriteString("@@")
		first := true
		for {
			t := p.peek()
			if t.kind == "ident" || t.kind == "kw" || (t.kind == "sym" && t.val == ".") {
				if !first && t.kind == "sym" {
					sb.WriteString(t.val)
					p.next()
					continue
				}
				sb.WriteString(t.val)
				first = false
				p.next()
				continue
			}
			break
		}
		name := strings.ToLower(strings.TrimPrefix(sb.String(), "@"))
		name = strings.TrimPrefix(name, "session.")
		name = strings.TrimPrefix(name, "global.")
		val := mysqlCfg.get().Variables[name]
		if val == "" {
			switch name {
			case "version_comment":
				val = "tsumugi"
			case "character_set_server", "character_set_database", "character_set_connection", "character_set_results":
				val = "utf8mb4"
			case "collation_server", "collation_database", "collation_connection":
				val = "utf8mb4_unicode_ci"
			case "lower_case_table_names":
				val = "0"
			case "max_allowed_packet":
				val = "67108864"
			case "time_zone":
				val = "SYSTEM"
			case "version":
				val = mysqlCfg.get().Version
			case "have_ssl":
				val = "DISABLED"
			}
		}
		columns = []string{"@" + strings.TrimPrefix(name, "@")}
		rows = [][]interface{}{{val}}
		return
	}
	// 函数调用：SELECT VERSION(), NOW(), DATABASE(), CURRENT_USER(), CONNECTION_ID() 等
	if t := p.peek(); t.kind == "ident" {
		fn := strings.ToUpper(t.val)
		// 需要紧跟 (
		save := p.i
		p.next()
		// COUNT 聚合交给 parseSelect 的 countSelect 专门路径处理
		if fn == "COUNT" {
			p.i = save
			return
		}
		if p.peek().kind == "sym" && p.peek().val == "(" {
			p.next()
			p.matchSym(")") // 可选参数，忽略内容
			for p.matchSym(",") {
				p.next()
				p.matchSym(")")
			}
			columns = []string{strings.ToLower(fn)}
			switch fn {
			case "VERSION":
				rows = [][]interface{}{{mysqlCfg.get().Version}}
			case "NOW", "CURRENT_TIMESTAMP", "CURRENT_TIME":
				rows = [][]interface{}{{time.Now().Format("2006-01-02 15:04:05")}}
			case "DATABASE", "SCHEMA":
				rows = [][]interface{}{{db.getCurDB()}}
			case "CURRENT_USER", "USER", "SESSION_USER":
				usr := ""
				if len(mysqlCfg.get().Users) > 0 {
					usr = mysqlCfg.get().Users[0].User + "@" + mysqlCfg.get().Users[0].Host
				}
				rows = [][]interface{}{{usr}}
			case "CONNECTION_ID":
				rows = [][]interface{}{{int64(1)}}
			case "COLLATION":
				rows = [][]interface{}{{"utf8mb4_unicode_ci"}}
			case "CHARSET":
				rows = [][]interface{}{{"utf8mb4"}}
			default:
				rows = [][]interface{}{{""}}
			}
			return
		}
		p.i = save
	}
	return
}
