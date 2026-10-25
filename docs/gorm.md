# 用 GORM 访问 Tsumugi：为什么没有「Tsumugi 方言」

> 这份文档可以直接转给同事阅读。结论先说：**Tsumugi 没有自己的 GORM 方言，也不需要有**——
> 直接用 `gorm.io/driver/mysql` 即可；只有在你想让 GORM 走 Tsumugi 的原生能力时，才值得写一个 dialector。

## 1. 先分清两个「协议」

| 层次 | 是什么 | Tsumugi 的情况 |
| --- | --- | --- |
| **线协议（wire protocol）** | 客户端与数据库之间的字节级通信：握手、认证、命令包、结果包 | Tsumugi 实现了 **MySQL 线协议**（`mysql_port`，默认 3309），另外还有自研的二进制协议（9999） |
| **SQL 方言** | 同一套 SQL 语法在不同数据库里的方言差异（`LIMIT` vs `TOP`、`RETURNING`、`ON CONFLICT` vs `ON DUPLICATE KEY`） | Tsumugi 解析的是 **MySQL 方言的 SQL 子集** |

GORM 的 `dialector` 解决的是**第二层**（SQL 方言 + 类型映射 + 迁移器）；
第一层（线协议）由 `database/sql` 驱动解决。

## 2. GORM 的 mysql dialector 实际做了什么

```go
import (
    "gorm.io/driver/mysql"           // ① 方言层
    _ "github.com/go-sql-driver/mysql" // ② 驱动层：真正讲 MySQL 线协议
)
```

- `gorm.io/driver/mysql` 负责：SQL 生成（反引号、`?` 占位符、`ON DUPLICATE KEY UPDATE`、`LIMIT n,m`）、Go 类型↔列类型映射、**Migrator**（AutoMigrate 的建表/改列/建索引）、错误码映射
- `github.com/go-sql-driver/mysql` 负责：TCP 连接、`mysql_native_password` 握手、`COM_QUERY` / 预处理语句、结果集解码

因为 `gorm.io/driver/mysql` 的底层是 **MySQL 线协议**，所以**任何说 MySQL 协议的数据库都能直接被 GORM 访问**——
Tsumugi 就是其中之一。GORM 并不知道、也不需要知道对面是 MySQL、TiDB 还是 Tsumugi。

```
   你的代码
     │  gorm.DB.Create / Where / AutoMigrate
     ▼
 gorm.io/gorm（通用 ORM 核心）
     │  按 Dialector 生成 SQL
     ▼
 gorm.io/driver/mysql（方言：引号/占位符/类型/Migrator）
     │  database/sql
     ▼
 github.com/go-sql-driver/mysql（驱动：MySQL 线协议）
     │  TCP
     ▼
 Tsumugi :3309（MySQL 线协议服务端）→ 引擎
```

## 3. 实测结论（gorm v1.25.12 + driver/mysql v1.5.7）

| 能力 | 结果 |
| --- | --- |
| `gorm.Open` + `Ping` | ✅ |
| `SELECT VERSION()` | 返回 `8.0.36-Tsumugi` |
| `AutoMigrate(&Model{})` | ✅（简单模型） |
| `Create` / 批量 `Create` / `First` / `Where` / `Count` / `Save` / `Delete` | ✅ |
| `db.Transaction(...)` + 回滚 | ✅（回滚后数据确实不存在） |

用法：

```go
db, err := gorm.Open(mysql.New(mysql.Config{
    DSN: "root:<密码>@tcp(127.0.0.1:3309)/?timeout=5s&charset=utf8mb4&parseTime=true",
}), &gorm.Config{})
```

## 4. 限制（用之前要知道）

| 限制 | 说明 |
| --- | --- |
| 列类型只有 3 种 | `INT/INTEGER/INT64/BIGINT`、`VARCHAR/STRING/TEXT/CHAR`、`BOOL/BOOLEAN` |
| 没有时间/浮点/decimal/json/blob 类型 | 模型里别用 `time.Time`、`float64`、`[]byte`；`gorm.Model`（含 `CreatedAt time.Time`、`DeletedAt`）也不能用。时间请用 `int64` 存 Unix 秒/纳秒 |
| 主键只能是单列整型 | 复合主键只取第一列，没有自增，要自己分配 ID |
| 无外键/CHECK 约束 | DDL 里会被忽略；行内 `UNIQUE` 也忽略，表级 `KEY/UNIQUE(...)` 会注册成二级索引 |
| 没有 SAVEPOINT | 事务只有 BEGIN/COMMIT/ROLLBACK |
| 并发写同一行 | 后提交方拿到 `conflict: row N version changed`（GORM 原样透传，需要自己重试） |
| 无 TLS | 协议端口按内网使用 |

## 5. 什么情况下才值得写一个 `gorm.io/driver/tsumugi`

写方言不是为了「能连上」（已经能连），而是为了下面这些**只有自己写才拿得到**的东西：

1. **让 GORM 走 Tsumugi 原生协议**：Tsumugi 的二进制协议（9999）比 SQL 解析路径快，但 GORM 只认 SQL，dialector 帮不上这个忙——要利用原生能力得写自定义 `gorm.Dialector` + 自定义 `ConnPool`
2. **规避 MySQL 方言里 Tsumugi 不支持的东西**：例如 MySQL 专属 DDL（`ENGINE=InnoDB`、`ON UPDATE CURRENT_TIMESTAMP`）、`information_schema` 深度查询
3. **提供 Tsumugi 特有的迁移策略**：TTL 列、整表原子 ALTER、冲突重试语义
4. **类型映射更贴合**：把 `time.Time` 映射成 `INT` 纳秒列、把 `float64` 映射成 `VARCHAR`，让现有模型直接可用

如果真要写，工作量大致是：

| 要覆写 | 内容 |
| --- | --- |
| `Migrator` | 跳过 MySQL 专属 DDL；`AutoMigrate` 只生成 Tsumugi 支持的 `CREATE TABLE` / `ALTER` 变体 |
| `DataTypeOf` | Go 类型 → `INT` / `VARCHAR` / `BOOL` 的映射（并拒绝无法映射的类型，给出明确报错） |
| `QuoteTo` | 保留反引号（与 Tsumugi 解析器一致） |
| `ClauseBuilders` | `ON DUPLICATE KEY UPDATE`（Tsumugi 已支持，可直接沿用） |
| `ErrorTranslator` | 把 `conflict: row N version changed` 翻译成可重试的错误类型 |

测试建议放在 `interop/`（独立 Go module，不污染主 module 的「零第三方依赖」），沿用现有
`go-sql-driver/mysql` 的真实驱动测试骨架，再加一层 GORM 用例。

## 6. 一句话版本（转述用）

> GORM 的「方言」是 SQL 生成层的适配器，而 Tsumugi 提供的是 MySQL 线协议。
> 线协议由 `go-sql-driver/mysql` 负责，所以 Tsumugi 直接用现成的 `gorm.io/driver/mysql` 就能被 GORM 访问，
> 不需要、也不建议再写一个方言；只有想用 Tsumugi 原生二进制协议、或想让 GORM 自动适配 Tsumugi 那套
> 只有 INT/VARCHAR/BOOL 的类型与整表 ALTER 语义时，才值得单独写一个 dialector。
