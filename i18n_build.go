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
// 字典拆分成多个文件维护（i18n_dash_*.go / i18n_wiz_*.go），统一在此合并；
// LANG_CODES 与语言工具函数（curLang/langName/t）也在注入 JS 中生成，
// 是前端唯一的语言逻辑来源（新增语言只需改 i18n_langs.go 的 langList）。
// 合并与语言函数生成在首次渲染时惰性执行并缓存（sync.Once）。

func dictToJS(d map[string]map[string]string) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// i18nCommonJS 三个页面共享的语言工具函数，随注入 JS 一并下发。
// LANG_CODES 由 I18N_LANGS 派生，避免语言列表在前端重复维护。
const i18nCommonJS = `
var LANG_CODES=(I18N_LANGS||[]).map(function(l){return l.code;});
function curLang(){var v=localStorage.getItem('tsumugi_lang');if(v&&LANG_CODES.indexOf(v)>=0)return v;var n=(navigator.language||'zh').toLowerCase().split('-')[0];return LANG_CODES.indexOf(n)>=0?n:'zh';}
function langName(c){for(var i=0;i<(I18N_LANGS||[]).length;i++){if(I18N_LANGS[i].code===c)return I18N_LANGS[i].name;}return c;}
function t(k){
  var v=I18N_TEXT[k];var c=curLang();
  var s=v?(v[c]||v.en||v.zh||('['+k+']')):('['+k+']');
  var args=Array.prototype.slice.call(arguments,1);
  return s.replace(/\{(\d+)\}/g,function(_,n){return args[+n]!=null?args[+n]:'{'+n+'}';});
}
`

// buildI18nJS 返回可直接注入 <script> 的 i18n JS 文本。
func buildI18nJS(d map[string]map[string]string) string {
	return "var I18N_TEXT=" + dictToJS(d) + ";\nvar I18N_LANGS=" + langsJSON() + ";\n" + i18nCommonJS
}

var (
	dictOnce   sync.Once
	dashDictJS string
	wizDictJS  string
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
