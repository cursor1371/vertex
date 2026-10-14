package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/api"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

func testPAASEnvironment() paasEnvironment {
	return paasEnvironment{
		AdminPassword: `test-password-with-"quote"-and-\slash`,
		APIKey:        "sk-" + strings.Repeat("a", 32),
	}
}

func TestValidateRulesEnvironment(t *testing.T) {
	hash := rulesHash()
	for _, tc := range []struct {
		name    string
		value   string
		present bool
		wantOK  bool
		wantErr bool
	}{
		{"absent", "", false, false, false},
		{"valid", hash, true, true, false},
		{"whitespace", " \n" + hash + "\n", true, true, false},
		{"empty", "", true, false, true},
		{"mismatch", "private-mistyped-value", true, false, true},
		{"substring", "prefix" + hash, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := validateRulesEnvironment(tc.value, tc.present, hash)
			if ok != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if err != nil && tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatal("error leaked invalid environment value")
			}
		})
	}
}

func TestBuildPAASConfigDefaultsAndPriority(t *testing.T) {
	env := testPAASEnvironment()
	cfg, err := buildPAASConfig([]byte(`{"parallel_pool_size":15}`), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PortAPI != 2156 || cfg.MaxConcurrent != 4 || cfg.ParallelPoolSize != 2 ||
		cfg.MaxRetries != 2 || cfg.RequestTotalTimeoutSeconds != 240 ||
		cfg.RecaptchaTokenPrefetch || cfg.HTTPLogEnabled || cfg.OpenAISSEHeartbeatSeconds != 15 {
		t.Fatal("unexpected PaaS defaults")
	}
	env.Port = "10000"
	env.ConfigJSON = `{"max_concurrent":2,"parallel_pool_size":1,"max_retries":0,"PORT_API":9000,"ADMIN_PASSWORD":"ignored"}`
	cfg, err = buildPAASConfig([]byte(`{}`), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PortAPI != 10000 || cfg.AdminPassword != env.AdminPassword ||
		cfg.MaxConcurrent != 2 || cfg.ParallelPoolSize != 1 || cfg.MaxRetries != 0 {
		t.Fatal("environment/config priority is incorrect")
	}
}

func TestBuildPAASConfigRejectsInvalidInput(t *testing.T) {
	for _, port := range []string{"0", "80", "65536", "-1", "+10000", "1e4", "10000\n", "99999999999999999999"} {
		t.Run("port_"+port, func(t *testing.T) {
			env := testPAASEnvironment()
			env.Port = port
			if _, err := buildPAASConfig([]byte(`{}`), env); err == nil {
				t.Fatal("invalid port accepted")
			}
		})
	}
	for _, raw := range []string{
		`[]`, `null`, `{}`, `{"max_concurrent":"private-input"}`,
		`{"max_concurrent":1.5}`, `{"typo_field":"private-input"}`,
		`{"max_concurrent":null}`, `{} {}`, `{"max_concurrent":`,
	} {
		t.Run("json_"+raw, func(t *testing.T) {
			env := testPAASEnvironment()
			env.ConfigJSON = raw
			_, err := buildPAASConfig([]byte(`{}`), env)
			if raw == `{}` {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid JSON configuration accepted")
			}
			if strings.Contains(err.Error(), "private-input") {
				t.Fatal("error leaked configuration value")
			}
		})
	}
	for _, password := range []string{"", "short", strings.Repeat(" ", 16), " abcdefghijklmnop", "abcdefghijklmnop\n",
		strings.Repeat("a", 1025), strings.Repeat("a", 16) + "\xff"} {
		env := testPAASEnvironment()
		env.AdminPassword = password
		if _, err := buildPAASConfig([]byte(`{}`), env); err == nil {
			t.Fatal("invalid password accepted")
		}
	}
	for _, key := range []string{"", "sk-your-key-here", "sk-" + strings.Repeat("a", 31),
		"sk-" + strings.Repeat("a", 257), "sk-" + strings.Repeat("a", 32) + "\nother:sk-evil"} {
		env := testPAASEnvironment()
		env.APIKey = key
		if _, err := buildPAASConfig([]byte(`{}`), env); err == nil {
			t.Fatal("invalid API key accepted")
		}
	}
	env := testPAASEnvironment()
	env.ConfigJSON = strings.Repeat(" ", maxBootstrapConfigBytes+1)
	if _, err := buildPAASConfig([]byte(`{}`), env); err == nil {
		t.Fatal("oversized configuration accepted")
	}
}

func TestBootstrapPAASFiles(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"config.example.json": `{}`, "models.json": `{"models":["test-model"]}`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := testPAASEnvironment()
	if err := bootstrapPAASFiles(root, env); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminPassword != env.AdminPassword {
		t.Fatal("password JSON escaping changed its value")
	}
	keysPath := filepath.Join(root, "config", "api_keys.txt")
	keys, err := os.ReadFile(keysPath)
	if err != nil || string(keys) != "paas:"+env.APIKey+":environment bootstrap\n" {
		t.Fatal("unexpected key file format")
	}
	t.Setenv("VPROXY_API_KEYS", keysPath)
	keyManager := api.NewAPIKeyManager()
	if !keyManager.LoadKeys() || keyManager.Count() != 1 || !keyManager.ValidateKey(env.APIKey) {
		t.Fatal("generated key file is incompatible with APIKeyManager")
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{cfgPath, keysPath} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("bootstrap files must have mode 0600")
			}
		}
	}
	// 重复执行刷新启动文件，但不能清空已有用户数据。
	sentinel := filepath.Join(root, "config", "data.db")
	if err := os.WriteFile(sentinel, []byte("must-remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Port = "10000"
	if err := bootstrapPAASFiles(root, env); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(sentinel)
	if err != nil || string(data) != "must-remain" {
		t.Fatal("bootstrap removed existing database")
	}
	data, err = os.ReadFile(cfgPath)
	if err != nil || json.Unmarshal(data, &cfg) != nil || cfg.PortAPI != 10000 {
		t.Fatal("restart did not refresh configuration")
	}
	before := string(data)
	env.ConfigJSON = `{"max_concurrent":"bad"}`
	if err := bootstrapPAASFiles(root, env); err == nil {
		t.Fatal("invalid bootstrap should fail")
	}
	data, err = os.ReadFile(cfgPath)
	if err != nil || string(data) != before {
		t.Fatal("invalid input changed existing configuration")
	}
	leftovers, err := filepath.Glob(filepath.Join(root, "config", ".bootstrap-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("temporary bootstrap files were not cleaned up")
	}
}

func TestPAASRepositoryTemplate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildPAASConfig(data, testPAASEnvironment()); err != nil {
		t.Fatal(err)
	}
}
