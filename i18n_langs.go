package main

import (
	"encoding/json"
	"strings"
)

// 支持的语言列表（按界面显示顺序）：共 10 种语言。
var langList = []string{"zh", "en", "ja", "ko", "fr", "de", "es", "pt", "ru", "vi"}

// langNative 各语言的原生名称（用于语言选择界面）。
var langNative = map[string]string{
	"zh": "中文",
	"en": "English",
	"ja": "日本語",
	"ko": "한국어",
	"fr": "Français",
	"de": "Deutsch",
	"es": "Español",
	"pt": "Português",
	"ru": "Русский",
	"vi": "Tiếng Việt",
}

// normalizeLang 将请求头语言（如 zh-CN / en-US）规范为支持的语言代码；
// 未知语言回退到 en，空值回退到 zh（服务端默认中文）。
func normalizeLang(s string) string {
	if s == "" {
		return "zh"
	}
	short := s
	if i := strings.IndexByte(s, '-'); i > 0 {
		short = s[:i]
	}
	for _, l := range langList {
		if l == short {
			return l
		}
	}
	if short == "en" {
		return "en"
	}
	return "zh"
}

// langsJSON 生成前端 I18N_LANGS 数组 JS（code + 原生名称 + 英文名）。
func langsJSON() string {
	arr := make([]map[string]string, 0, len(langList))
	enNames := map[string]string{
		"zh": "Chinese", "en": "English", "ja": "Japanese", "ko": "Korean",
		"fr": "French", "de": "German", "es": "Spanish", "pt": "Portuguese",
		"ru": "Russian", "vi": "Vietnamese",
	}
	for _, c := range langList {
		arr = append(arr, map[string]string{
			"code": c,
			"name": langNative[c],
			"en":   enNames[c],
		})
	}
	b, _ := json.Marshal(arr)
	return string(b)
}