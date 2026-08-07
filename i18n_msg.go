package main

import (
	"fmt"
	"net/http"
)

// ==================== 服务端 API 消息 i18n ====================
// 前端通过请求头 X-Lang 声明语言（zh/en），服务端据此返回对应语言的消息文本。

// msgDict 服务端返回给前端的关键消息字典。
var msgDict = map[string][2]string{
	"session_expired":  {"未登录或会话已过期", "not logged in or session expired"},
	"bad_credentials":  {"用户名或密码错误", "invalid username or password"},
	"settings_saved":   {"已保存，部分变更需重启服务生效", "saved; some changes need a restart"},
	"settings_load_err": {"加载设置失败", "failed to load settings"},
	"compact_done":    {"WAL 整理完成", "WAL compacted"},
}

// reqLang 从请求头提取语言："en" 或 "zh"。
func reqLang(r *http.Request) string {
	if r != nil && r.Header.Get("X-Lang") == "en" {
		return "en"
	}
	return "zh"
}

// trMsg 按语言取消息。
func trMsg(lang, key string, args ...interface{}) string {
	pair, ok := msgDict[key]
	if !ok {
		return key
	}
	var s string
	if lang == "en" {
		s = pair[1]
	} else {
		s = pair[0]
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}
