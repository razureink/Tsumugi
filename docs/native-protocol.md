# Tsumugi 原生二进制协议（Native Binary Protocol）

Tsumugi 的主协议：一条 TCP 长连接、1 字节命令码 + 二进制载荷。默认端口 **9999**（`config/tsumugi.json` 的 `port`，环境变量 `TSUMUGI_PORT`）。

- 所有多字节整数为 **Big-Endian**（网络字节序）
- 字符串一律 UTF-8，长度前缀为 **u16**（≤65535）；结果集单元格长度前缀为 **u32**
- 无 TLS：只用于内网/本机，或经 SSH 隧道、反向代理转发

---

## 1. 连接与认证

```
客户端 ──TCP──> host:9999
   │  CMD_AUTH(10)   ← 必须是第一条命令
   │<── 0x01 OK
   │  CMD_QUERY_SQL(35) / CMD_INSERT(30) / ...
```

规则（服务端实际行为）：

| 约束 | 说明 |
| --- | --- |
| 首条命令必须是 `CMD_AUTH` | 其他命令一律回 `0x02 authentication required` 并**断开** |
| 认证失败即断开 | 同一连接不能连续试密码；失败计数按 `IP+用户名` 累计，超限后回 `too many failed attempts` 并断开 |
| 未认证连接最多空闲 30s | 认证后按 `idle_timeout_s`（默认 60s） |
| 连接上限 | `max_conns`，默认 512 |
| 会话内事务 | `CMD_BEGIN` 之后所有写命令自动携带该事务；连接断开时未提交事务自动回滚 |

---

## 2. 通用帧格式

### 请求

```
[cmd:1][payload...]
```

### 响应

| 码 | 常量 | 结构 |
| --- | --- | --- |
| `0x01` | RESP_OK | 单字节，无载荷 |
| `0x02` | RESP_ERR | `[msgLen:4][msg]`（**不是** code+msg） |
| `0x03` | RESP_VALUE | `[len:4][bytes]`（仅 `CMD_STATUS` 使用） |
| `0x04` | RESP_NOT_FOUND | 预留，当前未使用 |
| `0x05` | RESP_ROWS | 命令相关，见各命令 |
| `0x06` | RESP_TXN_ID | `[txnID:8]`（仅 `CMD_BEGIN`） |

> 只要收到 `0x02`，该请求就失败了，`msg` 是英文错误描述（如 `permission denied`、`table not found`、`checksum conflict`）。连接保持可用，除非错误来自 `AUTH`。

---

## 3. 字段值编码（`CMD_INSERT` / `CMD_UPDATE` / `CMD_SELECT` 条件共用）

```
[nameLen:2][name][valType:1][value]
```

| valType | 类型 | 后续 |
| --- | --- | --- |
| 1 | INT | `i64`（8 字节） |
| 2 | VARCHAR | `[len:2][bytes]`（≤65534 字节） |
| 3 | BOOL | `[b:1]`，0=false，1=true |

建表/ALTER 用的类型码相同：`1=INT, 2=VARCHAR, 3=BOOL`，另带 `[len:4]` 作为声明长度。

---

## 4. 权限模型

| 命令 | 需要的权限 |
| --- | --- |
| `CREATE TABLE` / `DROP TABLE` / `ALTER TABLE` / `CREATE INDEX` | 该表的 `PermDDL(16)` 或 root |
| `INSERT` | `PermInsert(2)` |
| `UPDATE` | `PermUpdate(4)` |
| `DELETE` | `PermDelete(8)` |
| `SELECT` / `DESCRIBE` | `PermSelect(1)` |
| `QUERY_SQL` | 按解析后的语句类型逐条判定；非管理员不能写/DDL，不能 `USE` |
| `BACKUP` / `COMPACT` / `SET DURABILITY` / `GRANT` / `REVOKE` | 仅 root |
| 系统表 `__tables` / `__stats` 等 | 仅 root |

`GRANT/REVOKE` 的 `perm` 是位掩码：`1=SELECT, 2=INSERT, 4=UPDATE, 8=DELETE, 16=DDL`。

---

## 5. 命令总览

| 码 | 命令 | 状态 |
| --- | --- | --- |
| 10 | AUTH | 可用 |
| 11 | PING | 可用 |
| 20 | CREATE TABLE | 可用 |
| 21 | DROP TABLE | 可用 |
| 22 | DESCRIBE | 可用 |
| 23 | ALTER TABLE（仅 ADD COLUMN） | 可用 |
| 30 | INSERT | 可用 |
| 31 | SELECT（二进制条件查询） | 可用 |
| 32 | UPDATE（按主键） | 可用 |
| 33 | DELETE（按主键） | 可用 |
| 35 | QUERY_SQL（通用 SQL，推荐） | 可用 |
| 40 | BEGIN | 可用 |
| 41 | COMMIT | 可用 |
| 42 | ROLLBACK | 可用 |
| 50 | CREATE INDEX | 可用 |
| 55 | BATCH | **未实现**，固定回 `0x02 batch command not implemented` |
| 56 | BACKUP | 可用（root） |
| 57 | STATUS | 可用 |
| 58 | COMPACT | 可用（root） |
| 59 | SET DURABILITY | 可用（root） |
| 60 | CREATE PROC | **未实现**，回 `0x02 not implemented` |
| 61 | CALL PROC | **未实现** |
| 70 | CREATE VIEW | **未实现** |
| 80 | CREATE TRIGGER | **未实现** |
| 90 | GRANT | 可用（root） |
| 91 | REVOKE | 可用（root） |

> 写业务逻辑优先用 `CMD_QUERY_SQL`：一条命令覆盖所有语句，返回文本结果集，不需要客户端理解行字节布局。`CMD_INSERT/SELECT/UPDATE/DELETE` 是零解析开销的快捷通道，适合固定表结构的写入与主键查询。

---

## 6. 命令详解

### CMD_AUTH (10)

```
[u16 userLen][user][u16 passLen][pass]
```

- 成功：`0x01`
- 失败：`0x02 [msgLen:4][msg]`，随后服务端关闭连接

### CMD_PING (11)

无载荷 → `0x01`。

### CMD_QUERY_SQL (35)

```
[u16 sqlLen][sql utf-8 bytes]
```

SQL 长度受 u16 限制（≤65535 字节）；更长的语句请拆分为多条或改用协议命令。

**无结果集**（INSERT/UPDATE/DELETE/CREATE/DROP/SET/COMMIT…）：

```
[0x01][affected:8][msgLen:2][msg]
```

**有结果集**（SELECT/SHOW/DESCRIBE）：

```
[0x05]
[ncol:4]  ncol × [colLen:2][colName]
[nrow:4]
nrow × (
    [rowBytes:4]
    ncol × [cellLen:4][cell utf-8]
)
```

单元格语义：`NULL` 编码为长度 0 的空串；`BOOL` 为 `"1"`/`"0"`；整数/浮点/字符串按十进制或原文输出。`SHOW/DESCRIBE` 的列名可能与表达式一致（含 `AS` 别名）。

### CMD_INSERT (30) / CMD_UPDATE (32)

```
[u16 tableLen][table]
[i64 pk]
[i64 ttlSeconds]        // <=0 表示永不过期
[u16 fieldCount]
fieldCount × ([u16 nameLen][name][u8 valType][value])
```

- 响应：`0x01`
- `UPDATE` 以 pk 定位整行，未出现的字段按类型零值/空串处理；不存在的主键返回 `0x02` 错误
- 在事务中调用时自动带事务 ID，提交失败会返回 `conflict: row N version changed`

### CMD_DELETE (33)

```
[u16 tableLen][table][i64 pk]
```

→ `0x01`；主键不存在返回错误。

### CMD_SELECT (31)

```
[u16 tableLen][table]
[u16 condCount]
condCount × ([u16 nameLen][name][u8 valType][value])   // 等值条件，AND 连接
[u8 hasMin][i64 minKey?]
[u8 hasMax][i64 maxKey?]
```

响应：

```
[0x05][nrow:4]  nrow × ([len:4][raw row bytes])
```

`raw row bytes` 是引擎内部行编码，客户端需要按表结构解码：

```
[version:8][expireAt:8]
fields 按建表顺序：
  INT     → [i64:8]
  VARCHAR → [len:2][bytes]，0xFFFF 表示 NULL
  BOOL    → [b:1]
```

- 无等值条件时最多返回 **100 行**（`minKey/maxKey` 主键区间下推后取前 100 条）；有条件时返回全部命中行
- 想拿可直接阅读的文本行，用 `CMD_QUERY_SQL`
- 系统表 `__*` 仅 root 可读，行体为 `[i64 0][i64 0][u16 jsonLen][json]`

### CMD_CREATE_TABLE (20)

```
[u16 tableLen][table]
[u16 fieldCount]  fieldCount × ([u16 fNameLen][fName][u8 fType][u32 fLen])
[u16 pkLen][pk]
[u16 idxCount]    idxCount × ([u16 idxNameLen][idxName][u16 fieldLen][field])
```

→ `0x01`；需要该表 `PermDDL`。主键字段名必须出现在字段列表中。

### CMD_DROP_TABLE (21)

```
[u16 nameLen][name]
```

→ `0x01`；需要 `PermDDL`。同时删除该表的所有 GRANT，同名重建不会继承旧权限。

### CMD_DESCRIBE (22)

```
[u16 nameLen][name]
```

响应：

```
[0x01][nFields:2][nIndexes:2]
nFields  × ([u16 nameLen][name][u8 type][u32 len])
nIndexes × ([u16 idxNameLen][idxName][u16 fieldLen][field])
```

需要该表 `PermSelect`（无权限账号不能枚举表结构）。

### CMD_ALTER_TABLE (23)

```
[u16 tableLen][table][u16 fieldLen][field][u8 type][u32 len]
```

→ `0x01`；只支持 ADD COLUMN。底层是「新 meta + 全表行」的单条 WAL 记录，重放原子生效。

### CMD_CREATE_INDEX (50)

```
[u16 tableLen][table][u16 idxNameLen][idxName][u16 fieldLen][field]
```

→ `0x01`；索引名已存在 / 字段不存在返回 `0x02`。

### CMD_BEGIN (40) / COMMIT (41) / ROLLBACK (42)

- `BEGIN` → `0x06 [txnID:8]`；会话已有事务时返回错误（不会覆盖旧事务）
- `COMMIT` / `ROLLBACK` → `0x01`；无活动事务时报错
- 提交采用「本事务写出的行版本 == 当前行版本」校验：并发改同一行时后提交方失败并只回滚自己写的行

### CMD_BACKUP (56) / CMD_COMPACT (58)

无载荷 → `0x01`；仅 root。备份在 `backup/tsumugi_<时间戳>.wal`，压缩把 WAL 重写为当前内存态快照（v3 分帧格式）。

### CMD_STATUS (57)

```
[0x03][len:4][JSON]
```

JSON 字段来自运行时统计，并附带 `durability`（`batch`/`fsync`）。

### CMD_SET_DURABILITY (59)

```
[u16 modeLen][mode]   // "batch" 或 "fsync"
```

→ `0x01`；仅 root，其他取值返回 `durability must be batch or fsync`。

### CMD_GRANT (90) / CMD_REVOKE (91)

```
[u16 tableLen][table][u16 userLen][user][u8 perm]
```

→ `0x01`；仅 root。`perm` 位掩码见第 4 节。

---

## 7. Go 客户端示例

下面是最小可用客户端（AUTH → 写入 → 查询），可直接放进 `package main` 运行：

```go
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	cmdAuth     = 10
	cmdQuerySQL = 35
	respOK      = 0x01
	respErr     = 0x02
	respRows    = 0x05
)

type client struct {
	conn net.Conn
}

func appendU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.BigEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func dial(addr, user, pass string) (*client, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	pkt := []byte{cmdAuth}
	pkt = appendU16(pkt, uint16(len(user)))
	pkt = append(pkt, user...)
	pkt = appendU16(pkt, uint16(len(pass)))
	pkt = append(pkt, pass...)
	if _, err := conn.Write(pkt); err != nil {
		conn.Close()
		return nil, err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(conn, code); err != nil {
		conn.Close()
		return nil, err
	}
	if code[0] != respOK {
		msg, _ := readErr(conn)
		conn.Close()
		return nil, fmt.Errorf("auth: %s", msg)
	}
	return &client{conn: conn}, nil
}

func readErr(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Query 执行一条 SQL；有结果集时返回列名与行（所有单元格为字符串）。
func (c *client) Query(sql string) (cols []string, rows [][]string, err error) {
	pkt := []byte{cmdQuerySQL}
	pkt = appendU16(pkt, uint16(len(sql)))
	pkt = append(pkt, sql...)
	if _, err := c.conn.Write(pkt); err != nil {
		return nil, nil, err
	}
	br := make([]byte, 1)
	if _, err := io.ReadFull(c.conn, br); err != nil {
		return nil, nil, err
	}
	switch br[0] {
	case respErr:
		msg, e := readErr(c.conn)
		return nil, nil, fmt.Errorf("sql: %s (%v)", msg, e)
	case respOK:
		var affected uint64
		binary.Read(c.conn, binary.BigEndian, &affected)
		var msgLen uint16
		binary.Read(c.conn, binary.BigEndian, &msgLen)
		msg := make([]byte, msgLen)
		io.ReadFull(c.conn, msg)
		return nil, nil, nil
	case respRows:
		var ncol uint32
		binary.Read(c.conn, binary.BigEndian, &ncol)
		cols = make([]string, ncol)
		for i := range cols {
			var l uint16
			binary.Read(c.conn, binary.BigEndian, &l)
			b := make([]byte, l)
			io.ReadFull(c.conn, b)
			cols[i] = string(b)
		}
		var nrow uint32
		binary.Read(c.conn, binary.BigEndian, &nrow)
		rows = make([][]string, nrow)
		for i := range rows {
			var rowLen uint32
			binary.Read(c.conn, binary.BigEndian, &rowLen)
			row := make([]string, ncol)
			for j := range row {
				var cellLen uint32
				binary.Read(c.conn, binary.BigEndian, &cellLen)
				b := make([]byte, cellLen)
				io.ReadFull(c.conn, b)
				row[j] = string(b)
			}
			rows[i] = row
		}
		return cols, rows, nil
	}
	return nil, nil, fmt.Errorf("unexpected response code %d", br[0])
}

func main() {
	c, err := dial("127.0.0.1:9999", "root", "your-password")
	if err != nil {
		panic(err)
	}
	defer c.conn.Close()

	if _, _, err := c.Query(`CREATE TABLE IF NOT EXISTS users (id INT PRIMARY KEY, name VARCHAR(40), vip BOOL)`); err != nil {
		panic(err)
	}
	if _, _, err := c.Query(`INSERT INTO users (id, name, vip) VALUES (1, 'Alice', true)`); err != nil {
		panic(err)
	}
	cols, rows, err := c.Query(`SELECT id, name FROM users ORDER BY id`)
	if err != nil {
		panic(err)
	}
	fmt.Println(cols)
	for _, r := range rows {
		fmt.Println(r)
	}
}
```

仓库内已有完整实现可参考：`cmd/tsumugi-cli/main.go`（交互式 CLI，含事务、`backup`/`compact`/`status`、CSV 导入）。

---

## 8. 常见坑

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 发任何命令都收到 `authentication required` 并断连 | 首条不是 `CMD_AUTH` | 先认证，且认证失败必须重连 |
| 连续登录失败被拒 | `IP+用户名` 触发退避 | 等待退避窗口，或从面板/本机重置 |
| `permission denied` | 账号没有该表对应权限 | root 执行 `GRANT`（或面板授权） |
| `0x02 not implemented` | 用了 BATCH/PROC/VIEW/TRIGGER | 改用 `CMD_QUERY_SQL` 逐条执行 |
| 读 `CMD_SELECT` 结果乱码 | 返回的是带 `[version][expireAt]` 前缀的内部行编码 | 用 `CMD_QUERY_SQL`，或按第 6 节布局解码 |
| 大 SQL 被拒 | u16 长度上限 65535 | 拆分为多条语句 |
| 连接 60s 后被断开 | `idle_timeout_s` | 定时 `CMD_PING` 保活，或调大配置 |
