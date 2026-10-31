package main

// ==================== MySQL 错误码对齐（A7） ====================
// 之前所有 SQL 执行失败一律返回 1064（语法错误），于是：
//   - 唯一键冲突拿不到 1062，ORM 的 upsert / 重试逻辑全部失效；
//   - 死锁拿不到 1213，连接池不会做退避重试；
//   - 表不存在拿不到 1146，迁移工具无法区分"缺表"与"写错语句"。
//
// 这里把内部错误映射到 MySQL 的 (errno, SQLSTATE) 组合。
// 匹配策略：优先用哨兵错误 errors.Is（稳），其次用消息前缀（内部大量使用
// fmt.Errorf 构造字符串，没有错误类型，只能按前缀兜底）。

import (
	"errors"
	"strings"
)

// MySQL 常用错误码（只列本项目会真实产生的）。
const (
	ERDupEntry            = 1062 // 唯一键冲突
	ERLockDeadlock        = 1213 // 死锁，事务被回滚
	ERLockWaitTimeout     = 1205 // 锁等待超时
	ERNoSuchTable         = 1146 // 表不存在
	ERTableExistsError    = 1050 // 表已存在
	ERBadFieldError       = 1054 // 未知列
	ERWrongValueCount     = 1136 // 列数与值数不匹配
	ERDBAccessDenied      = 1044 // 无权限（库/表级）
	ERColumnAccessDenied  = 1142 // 无权限（列级 SELECT 被拒）
	ERTruncatedWrongValue = 1366 // 值类型不匹配
	ERDataTooLong         = 1406 // 数据超长
	ERParseError          = 1064 // 语法错误（兜底）
	ERUnknownError        = 1105 // 未分类
)

// sqlStateFor 返回 errno 对应的 SQLSTATE。
func sqlStateFor(code int) string {
	switch code {
	case ERDupEntry:
		return "23000"
	case ERLockDeadlock:
		return "40001"
	case ERLockWaitTimeout:
		return "HY000"
	case ERNoSuchTable:
		return "42S02"
	case ERTableExistsError:
		return "42S01"
	case ERBadFieldError:
		return "42S22"
	case ERWrongValueCount:
		return "21S01"
	case ERDataTooLong:
		return "22001"
	case ERTruncatedWrongValue:
		return "HY000"
	case ERDBAccessDenied, ERColumnAccessDenied:
		return "42000"
	case ERParseError:
		return "42000"
	}
	return "HY000"
}

// mysqlErrno 把内部错误映射成 MySQL errno。
// 顺序很重要：越具体的判定放越前面（例如 "table ... not found" 要先于通用匹配）。
func mysqlErrno(err error) int {
	if err == nil {
		return 0
	}
	// 1) 哨兵错误
	if errors.Is(err, errPKExists) {
		return ERDupEntry
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	// 2) 事务冲突（本项目内部用 conflict: 前缀表示写冲突/死锁）
	if strings.HasPrefix(low, "conflict:") || strings.Contains(low, "deadlock") {
		return ERLockDeadlock
	}
	if strings.Contains(low, "lock wait timeout") {
		return ERLockWaitTimeout
	}
	// 3) 表/列/权限
	switch {
	case strings.Contains(low, "table") && strings.Contains(low, "not found"),
		strings.HasPrefix(low, "no such table"):
		return ERNoSuchTable
	case strings.Contains(low, "already exists"):
		return ERTableExistsError
	case strings.Contains(low, "no matching columns"),
		strings.Contains(low, "unknown column"),
		strings.Contains(low, "bad field"):
		return ERBadFieldError
	case strings.Contains(low, "column count mismatch"),
		strings.Contains(low, "wrong value count"):
		return ERWrongValueCount
	case strings.Contains(low, "column access denied"):
		// 列级 SELECT 被拒：1142（区别于表/库级的 1044）。
		return ERColumnAccessDenied
	case strings.Contains(low, "permission denied"),
		strings.Contains(low, "access denied"),
		strings.Contains(low, "admin-only"):
		return ERDBAccessDenied
	case strings.Contains(low, "primary key must be integer"),
		strings.Contains(low, "cannot convert"),
		strings.Contains(low, "type mismatch"):
		return ERTruncatedWrongValue
	case strings.Contains(low, "too long"),
		strings.Contains(low, "exceeds max length"):
		return ERDataTooLong
	}
	// 4) 兜底：解析错误
	return ERParseError
}

// mysqlErrText 生成 MySQL 风格的错误文本（客户端会原样展示给用户）。
func mysqlErrText(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errPKExists) {
		// MySQL: Duplicate entry 'x' for key 'PRIMARY'
		return "Duplicate entry '" + strings.TrimPrefix(err.Error(), errPKExists.Error()+": ") +
			"' for key 'PRIMARY'"
	}
	return err.Error()
}
