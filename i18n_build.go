package main

import (
	"encoding/json"
	"sync"
)

// ==================== 前端 i18n 字典与 JS 注入 ====================
// 各页面模板中放置 /*__I18N__*/ 标记，在渲染时替换为 buildI18nJS() 生成的 JS：
//
//	var I18N_TEXT = {key: {lang: "text", ...}, ...};
//	var I18N_LANGS = [{code, name, en}, ...];
//
// 字典拆分成多个文件维护（i18n_dash_*.go / i18n_wiz_*.go），统一在此合并。
//
// 注意：合并不能在 init() 中执行。i18n_wiz_terms.go 的 init() 负责把
// wizTermRows 组装成 wizDictTerms，而包内 init() 按文件名字典序执行
// （i18n_build.go 排在 i18n_wiz_terms.go 之前），若在此合并会导致合并时
// wizDictTerms 仍为 nil、条款文案丢失（前端显示 [term1]）。
// 因此改为渲染期惰性构建并缓存（sync.Once）。

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

var (
	dictOnce    sync.Once
	dashDictJS  string
	wizDictJS   string
)

// ensureI18nDicts 构建并缓存两套注入 JS。首次渲染时执行一次。
func ensureI18nDicts() {
	dictOnce.Do(func() {
		dashDictJS = buildI18nJS(mergeDicts(dashDictMonitor, dashDictAdmin, dashDictSettings, dashDictUsers))
		wizDictJS = buildI18nJS(mergeDicts(wizDictMain, wizDictTerms))
	})
}

// cachedDashDictJS 返回控制台/登录页注入 JS（首次调用时惰性构建）。
func cachedDashDictJS() string {
	ensureI18nDicts()
	return dashDictJS
}

// cachedWizDictJS 返回首次设置向导注入 JS（首次调用时惰性构建）。
func cachedWizDictJS() string {
	ensureI18nDicts()
	return wizDictJS
}

func mergeDicts(parts ...map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}