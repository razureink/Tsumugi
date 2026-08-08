package main

import (
	"fmt"
	"net/http"
)

// ==================== 服务端 API 消息 i18n ====================
// 前端通过请求头 X-Lang 声明语言（如 zh-CN / en / ja / ru），服务端据此返回对应语言的消息文本。
// 支持 langList 中的 10 种语言；未知语言回退中文。

// msgDict 服务端返回给用户消息字典：key -> lang -> 文本。
var msgDict = map[string]map[string]string{
	"zh": {
		"session_expired":   "未登录或会话已过期",
		"bad_credentials":   "用户名或密码错误",
		"settings_saved":    "设置已保存，部分变更需重启服务生效",
		"settings_load_err": "加载设置失败",
		"compact_done":      "WAL 整理完成",
	},
	"en": {
		"session_expired":   "not logged in or session expired",
		"bad_credentials":   "invalid username or password",
		"settings_saved":    "saved; some changes need a restart",
		"settings_load_err": "failed to load settings",
		"compact_done":      "WAL compacted",
	},
	"ja": {
		"session_expired":   "未ログイン、またはセッションが期限切れです",
		"bad_credentials":   "ユーザー名またはパスワードが正しくありません",
		"settings_saved":    "保存しました。一部の変更は再起動後に反映されます",
		"settings_load_err": "設定の読み込みに失敗しました",
		"compact_done":      "WAL の整理が完了しました",
	},
	"ko": {
		"session_expired":   "로그인되지 않았거나 세션이 만료되었습니다",
		"bad_credentials":   "사용자 이름 또는 비밀번호가 올바르지 않습니다",
		"settings_saved":    "저장되었습니다. 일부 변경은 재시작 후 적용됩니다",
		"settings_load_err": "설정을 불러오지 못했습니다",
		"compact_done":      "WAL 정리가 완료되었습니다",
	},
	"fr": {
		"session_expired":   "non connecté ou session expirée",
		"bad_credentials":   "nom d'utilisateur ou mot de passe invalide",
		"settings_saved":    "enregistré ; certains changements nécessitent un redémarrage",
		"settings_load_err": "échec du chargement des paramètres",
		"compact_done":      "WAL compacté",
	},
	"de": {
		"session_expired":   "nicht angemeldet oder Sitzung abgelaufen",
		"bad_credentials":   "Benutzername oder Passwort falsch",
		"settings_saved":    "gespeichert ; manche Änderungen erfordern einen Neustart",
		"settings_load_err": "Einstellungen konnten nicht geladen werden",
		"compact_done":      "WAL verdichtet",
	},
	"es": {
		"session_expired":   "no inició sesión o la sesión expiró",
		"bad_credentials":   "usuario o contraseña incorrectos",
		"settings_saved":    "guardado; algunos cambios requieren un reinicio",
		"settings_load_err": "no se pudieron cargar los ajustes",
		"compact_done":      "WAL compactado",
	},
	"pt": {
		"session_expired":   "não autenticado ou sessão expirada",
		"bad_credentials":   "utilizador ou palavra-passe inválidos",
		"settings_saved":    "guardado; algumas alterações exigem um reinício",
		"settings_load_err": "falha ao carregar as definições",
		"compact_done":      "WAL compactado",
	},
	"ru": {
		"session_expired":   "не выполнен вход или сеанс истёк",
		"bad_credentials":   "неверное имя пользователя или пароль",
		"settings_saved":    "сохранено ; некоторые изменения требуют перезапуска",
		"settings_load_err": "не удалось загрузить настройки",
		"compact_done":      "WAL сжат",
	},
	"vi": {
		"session_expired":   "chưa đăng nhập hoặc phiên đã hết hạn",
		"bad_credentials":   "tên người dùng hoặc mật khẩu không đúng",
		"settings_saved":    "đã lưu; một số thay đổi cần khởi động lại",
		"settings_load_err": "không tải được cài đặt",
		"compact_done":      "đã nén WAL",
	},
}

// reqLang 从请求头提取语言代码（支持 10 种语言，未知回退中文）。
func reqLang(r *http.Request) string {
	if r == nil {
		return "zh"
	}
	return normalizeLang(r.Header.Get("X-Lang"))
}

// trMsg 按语言取消息；语言未知或缺键时按需回退（en -> zh -> key）。
func trMsg(lang, key string, args ...interface{}) string {
	s := msgDict[lang][key]
	if s == "" && lang != "zh" {
		s = msgDict["zh"][key]
	}
	if s == "" {
		s = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}