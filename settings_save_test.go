package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSettingsSave 验证设置保存：即使 config 目录不存在也能自动创建并落盘。
func TestSettingsSave(t *testing.T) {
	// 用临时路径模拟 config 目录不存在
	tmp := t.TempDir()
	fakeCfg := filepath.Join(tmp, "nested", "tsumugi.json")

	body := `{"user":"root","password":"password","binary_port":9999,"durability":"batch"}`
	_ = body
	// 直接测 writeJSONFile 的父目录自动创建
	if err := writeJSONFile(fakeCfg, map[string]interface{}{"ok": true}); err != nil {
		t.Fatalf("writeJSONFile: %v", err)
	}
	if _, err := os.Stat(fakeCfg); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	var j map[string]interface{}
	dat, _ := os.ReadFile(fakeCfg)
	json.Unmarshal(dat, &j)
	if j["ok"] != true {
		t.Fatalf("bad content: %s", string(dat))
	}
	t.Log("settings save (mkdir parent) ok")
}