package main

import (
	"strings"
	"testing"
)

// TestI18nDictContainsTerms 守护修复 i18n_build 的 init() 早于 wizDictTerms
// 组装执行导致向导条款显示 [termN] 的缺陷（见 i18n_build.go 注释）。
func TestI18nDictContainsTerms(t *testing.T) {
	js := cachedWizDictJS()
	for i := 1; i <= 8; i++ {
		title := `"term` + itoa(i) + `":`
		body := `"term` + itoa(i) + `_body":`
		if !strings.Contains(js, title) {
			t.Fatalf("wizard dict missing term%d", i)
		}
		if !strings.Contains(js, body) {
			t.Fatalf("wizard dict missing term%d_body", i)
		}
	}
	if !strings.Contains(js, "Tsumugi") {
		t.Fatalf("wizard dict lacks expected term body content")
	}
	// 控制台字典应包含监控与账号模块的 key
	djs := cachedDashDictJS()
	for _, k := range []string{"navMonitor", "modeRW"} {
		if !strings.Contains(djs, `"`+k+`":`) {
			t.Fatalf("dashboard dict missing %q", k)
		}
	}
}
