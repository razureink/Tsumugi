package main

// wizDictTerms 首次设置向导中的服务条款（由 wizTermRows 组装，16 个 key × 10 种语言）。
var wizDictTerms map[string]map[string]string

func init() {
	wizDictTerms = make(map[string]map[string]string, len(wizTermRows))
	for _, r := range wizTermRows {
		wizDictTerms[r.code] = map[string]string{
			"zh": r.zh, "en": r.en, "ja": r.ja, "ko": r.ko, "fr": r.fr,
			"de": r.de, "es": r.es, "pt": r.pt, "ru": r.ru, "vi": r.vi,
		}
	}
}