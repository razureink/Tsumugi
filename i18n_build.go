package main

import "encoding/json"

// ==================== 前端 i18n 字典与 JS 注入 ====================
// 各页面模板中放置 /*__I18N__*/ 标记，在响渲染时替换为 buildI18nJS() 生成的 JS：
//
//	var I18N_TEXT = {key: {lang: "text", ...}, ...};
//	var I18N_LANGS = [{code, name, en}, ...];
//
// 字典拆分成多个文件维护（i18n_dash_*.go / i18n_wiz_*.go），统一在此合并。

func dictToJS(d map[string]map[string]string) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// buildI18nJS 返回可直接注入 <script> 的 i18n JS 文本。
func buildI18nJS(d map[string]map[string]string) string {
	return "var I18N_TEXT=" + dictToJS(d) + ";\nvar I18N_LANGS=" + langsJSON() + ";"
}

// dashDictJS 与 wizDictJS 为页面渲染时替换 /*__I18N__*/ 标记所用的缓存 JS。
var dashDictJS string
var wizDictJS string

// dashDict / wizDict 为合并后的完整字典。
var dashDict map[string]map[string]string
var wizDict map[string]map[string]string

func mergeDicts(parts ...map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

func init() {
	dashDict = mergeDicts(dashDictMonitor, dashDictAdmin, dashDictSettings, dashDictUsers)
	wizDict = mergeDicts(wizDictMain, wizDictTerms)
	dashDictJS = buildI18nJS(dashDict)
	wizDictJS = buildI18nJS(wizDict)
}