package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTrailingFixModels_DefaultsAndNormalization 验证尾部兼容模型清单：
// 默认值预填、WriteSettings 写入后归一化（Trim/去空/去重）、切片防御性拷贝。
func TestTrailingFixModels_DefaultsAndNormalization(t *testing.T) {
	t.Run("默认配置预填 3 个默认模型", func(t *testing.T) {
		cfg := StaticProvider(DefaultConfig())
		got := cfg.TrailingFixModels()
		want := []string{"gemini-3.5-flash-lite", "gemini-3.6-flash", "gemini-3.7-flash"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("TrailingFixModels()=%v, want %v", got, want)
		}
	})

	t.Run("WriteSettings 写入带空格/重复/空项后归一化去重并 trim", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		t.Setenv("VPROXY_CONFIG", path)
		InvalidateCache()

		if err := WriteSettings(map[string]any{
			"trailing_fix_models": []string{" gemini-3.6-flash ", "gemini-3.6-flash", ""},
		}); err != nil {
			t.Fatalf("WriteSettings: %v", err)
		}

		got := StaticProvider(Load()).TrailingFixModels()
		want := []string{"gemini-3.6-flash"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("TrailingFixModels()=%v, want %v", got, want)
		}

		InvalidateCache() // 清理，避免影响其它测试
	})

	t.Run("显式空数组被尊重不回填默认值", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		t.Setenv("VPROXY_CONFIG", path)
		if err := os.WriteFile(path, []byte(`{"trailing_fix_models":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		InvalidateCache()

		got := StaticProvider(Load()).TrailingFixModels()
		if len(got) != 0 {
			t.Fatalf("显式 [] 应得到空清单，got %v", got)
		}

		InvalidateCache() // 清理
	})

	t.Run("返回切片为防御性拷贝", func(t *testing.T) {
		cfg := StaticProvider(DefaultConfig())
		first := cfg.TrailingFixModels()
		first[0] = "mutated"
		second := cfg.TrailingFixModels()
		if second[0] == "mutated" {
			t.Fatalf("修改返回切片不应影响内部缓存，second[0]=%q", second[0])
		}
	})
}

// TestIPv6Policy_Defaults 验证 IPv6 出口策略三个字段的默认值为关闭/空。
func TestIPv6Policy_Defaults(t *testing.T) {
	cfg := StaticProvider(DefaultConfig())
	if cfg.ForceNodeIPv6() {
		t.Error("ForceNodeIPv6() 默认应为 false")
	}
	if cfg.HostIPv6Interface() != "" {
		t.Errorf("HostIPv6Interface() 默认应为空, got %q", cfg.HostIPv6Interface())
	}
	if cfg.HostIPv6Address() != "" {
		t.Errorf("HostIPv6Address() 默认应为空, got %q", cfg.HostIPv6Address())
	}
}

// TestExampleConfigMatchesDefaults 锁定 config.example.json 与 DefaultConfig() 的关键键一致：
// Load 在 config.json 不存在时会直接用 example 内容生成初始配置（config.go Load 头部），
// 两处漂移时"文档和代码说了不算，example 说了算"，新装行为与注释语义背离。
// 历史上 max_retries 曾三处冲突（代码 1 / example 10 / 指南 2 与 10），预算差 11 倍。
func TestExampleConfigMatchesDefaults(t *testing.T) {
	const rel = "../../../config/config.example.json"
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Skipf("找不到 %s（工作目录=%s）", rel, os.Getenv("PWD"))
	}
	var example map[string]any
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}

	def := DefaultConfig()
	cases := []struct {
		key  string
		want any
	}{
		{"max_retries", float64(def.MaxRetries)},
		{"parallel_pool_size", float64(def.ParallelPoolSize)},
		{"recaptcha_try_entry_or_direct", def.RecaptchaTryEntryOrDirect},
		{"recaptcha_token_prefetch", def.RecaptchaTokenPrefetch},
		{"stream_idle_timeout_seconds", float64(def.StreamIdleTimeoutSeconds)},
		{"request_timeout_seconds", float64(def.RequestTimeoutSeconds)},
		{"max_n", float64(def.MaxN)},
		{"rt_compensation_ms", float64(def.RTCompensationMs)},
	}
	for _, c := range cases {
		got, ok := example[c.key]
		if !ok {
			t.Errorf("example 缺少键 %q（新装部署将走代码默认，文档与实际行为背离）", c.key)
			continue
		}
		if got != c.want {
			t.Errorf("example %s=%v, want %v（与 DefaultConfig 不一致）", c.key, got, c.want)
		}
	}
}
