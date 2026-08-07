# Tsumugi Native Binary Protocol

Tsumugi 提供一套自定义的二进制 TCP 协议（默认端口 9999），用于高性能的客户端访问。
所有多字节整数采用 **Big-Endian** 编码。

## 连接流程

1. 客户端建立 TCP 连接到 `host:9999`（或配置的 `port`）
2. 发送 **CMD_AUTH**（必须为第一条命令）
3. 服务端回复 `0x01`（OK）或错误码
4. 连接就绪，可发送任意命令

---

## 通用帧格式

### 请求

每个请求以 1 字节 **命令码** 开头，后跟该命令特有的载荷。

```
[cmd:1][payload...]
```

### 响应

每个响应以 1 字节 **响应码** 开头：

| 响应码 | 常量 | 含义 |
|--------|------|------|
| `0x01` | RESP_OK | 操作成功 |
| `0x02` | RESP_ERR | 操作失败，后跟错误信息 |
| `0x03` | RESP_VALUE | 返回单个值（预留） |
| `0x04` | RESP_NOT_FOUND | 未找到（预留） |
| `0x05` | RESP_ROWS | 返回结果集 |
| `0x06` | RESP_TXN_ID | 事务 ID（BEGIN 返回） |

---

## 命令列表

### CMD_AUTH (10) — 认证

**请求载荷：**
```
[userLen:2][user bytes][passLen:2][pass bytes]
```

- `userLen` — 用户名长度（u16）
- `user` — 用户名（UTF-8）
- `passLen` — 密码长度（u16）
- `pass` — 密码（UTF-8）

**响应：**
- OK: `0x01`
- ERR: `0x02` + [code:2][msg]

---

### CMD_PING (11) — 心跳

**请求载荷：** 无

**响应：** `0x01`

---

### CMD_QUERY_SQL (35) — SQL 查询（推荐）

通用 SQL 执行入口，支持所有 SQL 语句（SELECT/INSERT/UPDATE/DELETE/CREATE/DROP/SET/BEGIN/COMMIT/ROLLBACK 等）。

**请求载荷：**
```
[sqlLen:2][sql bytes]
```

**响应（无结果集，如 INSERT/UPDATE/CREATE）：**
```
0x01 [affected:8] [msgLen:2][msg bytes]
```

**响应（有结果集，如 SELECT）：**
```
0x05 [ncol:4] [col1Len:2][col1] ... [nrow:4] [row1] ...
```

每行格式：
```
[fieldCount:4] [f1Len:4][f1] [f2Len:4][f2] ...
```

所有字段值均为 **字符串形式**（`fmt.Sprintf("%v", v)`）。

---

### CMD_INSERT (30) — 插入行

**请求载荷：**
```
[tableNameLen:2][tableName] [pk:8] [ttl:8] [fieldCount:2] [field1] ... [fieldN]
```

每个字段：
```
[nameLen:2][name] [valType:1][value]
```

`valType`：
| 值 | 类型 | 后续数据 |
|----|------|---------|
| 1 | INT | i64 (8 bytes) |
| 2 | VARCHAR | [len:2][bytes] |
| 3 | BOOL | [b:1] (0=false, 1=true) |

**响应：** `0x01`

---

### CMD_SELECT (31) — 条件查询

**请求载荷：**
```
[tableNameLen:2][tableName] [condCount:2] [cond1] ... [condN] [hasMin:1] [minKey:8?] [hasMax:1] [maxKey:8?]
```

每个条件同 INSERT 的字段格式（`nameLen:2 + name + valType:1 + value`）。

**响应：**
```
0x05 [nrow:4] [row1Len:4][row1 bytes] ...
```

每行 `row bytes` 为 `encodeRow()` 编码的二进制数据。

---

### CMD_UPDATE (32) — 更新行

**请求载荷：**
```
[tableNameLen:2][tableName] [pk:8] [ttl:8] [fieldCount:2] [field1] ... [fieldN]
```

格式同 CMD_INSERT。

**响应：** `0x01`

---

### CMD_DELETE (33) — 删除行

**请求载荷：**
```
[tableNameLen:2][tableName] [pk:8]
```

**响应：** `0x01`

---

### CMD_CREATE_TABLE (20) — 创建表

**请求载荷：**
```
[tableNameLen:2][tableName]
[fieldCount:2]
  [fNameLen:2][fName] [fType:1] [fLen:4]
  ...
[pkLen:2][pk]
[idxCount:2]
  [idxNameLen:2][idxName] [idxFieldLen:2][idxField]
  ...
```

`fType`：1=INT, 2=VARCHAR, 3=BOOL

**响应：** `0x01`

---

### CMD_DROP_TABLE (21) — 删除表

**请求载荷：**
```
[nameLen:2][name]
```

**响应：** `0x01`

---

### CMD_DESCRIBE (22) — 查看表结构

**请求载荷：**
```
[nameLen:2][name]
```

**响应：**
```
0x01 [fieldCount:2] [idxCount:2]
  每个字段: [nameLen:2][name] [type:1] [len:4]
  每个索引: [nameLen:2][name] [fieldLen:2][field]
```

---

### CMD_ALTER_TABLE (23) — ALTER TABLE ADD COLUMN

**请求载荷：**
```
[tableNameLen:2][tableName] [fieldNameLen:2][fieldName] [fType:1] [fLen:4]
```

**响应：** `0x01`

---

### CMD_BEGIN (40) — 开始事务

**请求载荷：** 无

**响应：**
```
0x06 [txnID:8]
```

---

### CMD_COMMIT (41) — 提交事务

**请求载荷：** 无

**响应：** `0x01`

---

### CMD_ROLLBACK (42) — 回滚事务

**请求载荷：** 无

**响应：** `0x01`

---

### CMD_CREATE_INDEX (50) — 创建索引

**请求载荷：**
```
[tableNameLen:2][tableName] [idxNameLen:2][idxName] [fieldNameLen:2][fieldName]
```

**响应：** `0x01`

---

### CMD_BATCH (55) — 批量插入

**请求载荷：**
```
[rowCount:4]
  每行: [tableNameLen:2][tableName] [pk:8] [ttl:8] [fieldCount:2] [fields...]
```

**响应：** `0x01`

---

### CMD_BACKUP (56) — 备份

**请求载荷：** 无

**响应：** `0x01`

---

### CMD_STATUS (57) — 状态查询

**请求载荷：** 无

**响应：**
```
0x05 [dataLen:4][JSON bytes]
```

JSON 包含内存使用、表数量、行数等运行时统计。

---

### CMD_COMPACT (58) — 压缩

**请求载荷：** 无

**响应：** `0x01`

---

### CMD_SET_DURABILITY (59) — 设置持久化模式

**请求载荷：**
```
[modeLen:2][mode bytes]  // "batch" 或 "fsync"
```

**响应：** `0x01`

---

### CMD_CREATE_PROC (60) — 创建存储过程

**请求载荷：**
```
[nameLen:2][name] [paramsLen:2][params] [bodyLen:2][body]
```

**响应：** `0x01`

---

### CMD_CALL_PROC (61) — 调用存储过程

**请求载荷：**
```
[nameLen:2][name] [argsLen:2][args]
```

**响应：** 同 CMD_QUERY_SQL（结果集或 OK）。

---

### CMD_CREATE_VIEW (70) — 创建视图

**请求载荷：**
```
[nameLen:2][name] [sqlLen:2][sql]
```

**响应：** `0x01`

---

### CMD_CREATE_TRIGGER (80) — 创建触发器

**请求载荷：**
```
[nameLen:2][name] [tableLen:2][table] [timingLen:2][timing] [eventLen:2][event] [bodyLen:2][body]
```

**响应：** `0x01`

---

### CMD_GRANT (90) — 授权

**请求载荷：**
```
[tableNameLen:2][tableName] [userLen:2][user] [perm:1]
```

`perm` 位掩码：1=SELECT, 2=INSERT, 4=UPDATE, 8=DELETE, 16=DDL

**响应：** `0x01`

---

### CMD_REVOKE (91) — 撤销授权

**请求载荷：**
```
[tableNameLen:2][tableName] [userLen:2][user] [perm:1]
```

**响应：** `0x01`

---

## 示例：使用 Go 客户端

```go
conn, _ := net.DialTimeout("tcp", "127.0.0.1:9999", 3*time.Second)

// 认证
var authPkt []byte
authPkt = append(authPkt, 10) // CMD_AUTH
authPkt = appendU16(authPkt, uint16(len("root")))
authPkt = append(authPkt, "root"...)
authPkt = appendU16(authPkt, uint16(len("password")))
authPkt = append(authPkt, "password"...)
conn.Write(authPkt)
resp := make([]byte, 1)
io.ReadFull(conn, resp) // 0x01 = OK

// SQL 查询
var sqlPkt []byte
sqlPkt = append(sqlPkt, 35) // CMD_QUERY_SQL
sql := "SELECT * FROM users ORDER BY id"
sqlPkt = appendU16(sqlPkt, uint16(len(sql)))
sqlPkt = append(sqlPkt, sql...)
conn.Write(sqlPkt)

// 读取结果集...
```

---

## 注意事项

- 所有整数采用 **Big-Endian**（网络字节序）
- 连接超时由服务端 `idle_timeout_s` 控制（默认 60 秒）
- 事务在连接级别维护，BEGIN 后的命令自动附加 txnID
- CMD_QUERY_SQL 支持完整的 SQL 语法，是与 CLI 工具交互的主要命令
- CMD_INSERT/SELECT/UPDATE/DELETE 是原生二进制 API，适合高性能场景
