package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

const maxBootstrapConfigBytes = 1 << 20

type paasEnvironment struct {
	Port          string
	AdminPassword string
	APIKey        string
	ConfigJSON    string
}

// validateRulesEnvironment 区分未配置与配置错误：显式错误不得回退旧同意文件。
// 不打印环境变量原值，避免误填密钥时将秘密写进日志。
func validateRulesEnvironment(value string, present bool, currentHash string) (bool, error) {
	if !present {
		return false, nil
	}
	if strings.TrimSpace(value) != currentHash || currentHash == "" {
		return false, fmt.Errorf("规则确认失败；请先运行 rules 阅读规则，再设置 VPROXY_ACCEPT_RULES_HASH=%s", currentHash)
	}
	return true, nil
}

func runPAASBootstrap() error {
	value, present := os.LookupEnv("VPROXY_ACCEPT_RULES_HASH")
	agreed, err := validateRulesEnvironment(value, present, rulesHash())
	if err != nil {
		return err
	}
	if !agreed {
		return errors.New("PaaS 启动必须显式设置 VPROXY_ACCEPT_RULES_HASH；使用 rules / rules-hash 查看规则及版本")
	}
	text, nodesPresent := os.LookupEnv("VPROXY_BOOTSTRAP_NODES")
	if _, err := parseBootstrapNodes(text, nodesPresent); err != nil {
		return err
	}
	return bootstrapPAASFiles("/app", paasEnvironment{
		Port:          os.Getenv("PORT"),
		AdminPassword: os.Getenv("VPROXY_ADMIN_PASSWORD"),
		APIKey:        os.Getenv("VPROXY_API_KEY"),
		ConfigJSON:    os.Getenv("VPROXY_CONFIG_JSON"),
	})
}

// buildPAASConfig 只构造配置，不访问配置缓存、数据库或网络。
// 优先级：代码默认值 < 镜像模板 < PaaS 默认值 < JSON 覆盖 < 端口/密码环境变量。
func buildPAASConfig(template []byte, env paasEnvironment) (config.AppConfig, error) {
	cfg := config.DefaultConfig()
	if err := decodeBootstrapConfig(template, &cfg); err != nil {
		return cfg, errors.New("镜像 config.example.json 非法或与当前配置类型不兼容")
	}
	cfg.MaxConcurrent = 4
	cfg.ParallelPoolSize = 2
	cfg.MaxRetries = 2
	cfg.MaxRequestMB = 8
	cfg.MaxSpillMB = 128
	cfg.RequestTotalTimeoutSeconds = 240
	cfg.HistoryRetentionDays = 1
	cfg.RaceLogRetentionDays = 1
	cfg.HTTPLogRetentionDays = 1
	cfg.HTTPLogEnabled = false
	cfg.RecaptchaTokenPrefetch = false
	cfg.OpenAISSEHeartbeatSeconds = 15

	if env.ConfigJSON != "" {
		if err := decodeBootstrapConfig([]byte(env.ConfigJSON), &cfg); err != nil {
			return cfg, errors.New("VPROXY_CONFIG_JSON 必须为合法配置对象：不接受未知字段、字段类型错误、null 值或超过 1 MiB 的内容")
		}
	}

	portText := env.Port
	if portText == "" {
		portText = "2156"
	}
	for _, r := range portText {
		if r < '0' || r > '9' {
			return cfg, errors.New("PORT 必须是 1024～65535 的十进制整数（PaaS 镜像以非 root 用户运行）")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1024 || port > 65535 {
		return cfg, errors.New("PORT 必须是 1024～65535 的十进制整数（PaaS 镜像以非 root 用户运行）")
	}
	if !utf8.ValidString(env.AdminPassword) || utf8.RuneCountInString(env.AdminPassword) < 16 || len(env.AdminPassword) > 1024 ||
		strings.TrimSpace(env.AdminPassword) != env.AdminPassword ||
		strings.IndexFunc(env.AdminPassword, unicode.IsControl) >= 0 {
		return cfg, errors.New("VPROXY_ADMIN_PASSWORD 必须是有效 UTF-8，至少 16 个字符、最多 1024 字节，不得包含首尾空白或控制字符")
	}
	if !validPAASAPIKey(env.APIKey) {
		return cfg, errors.New("VPROXY_API_KEY 必须以 sk- 开头，后接 32～256 个 ASCII 字母、数字、下划线或连字符")
	}
	// 必须在 JSON 解码之后赋值，避免大小写别名等覆盖平台端口与密码。
	cfg.PortAPI = port
	cfg.AdminPassword = env.AdminPassword
	return cfg, nil
}

func decodeBootstrapConfig(data []byte, cfg *config.AppConfig) error {
	if len(data) > maxBootstrapConfigBytes {
		return errors.New("配置过大")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("配置必须为单个 JSON 对象")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("配置字段不能为 null")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		// 原始 JSON 错误可能包含用户输入，不把它透传到平台日志。
		return errors.New("配置字段未知或类型不匹配")
	}
	return nil
}

func validPAASAPIKey(key string) bool {
	if !strings.HasPrefix(key, "sk-") || len(key) < 35 || len(key) > 259 {
		return false
	}
	for _, r := range key[3:] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// bootstrapPAASFiles 每次重建三份启动文件，但不删除 SQLite、节点、日志或用户资产。
// 所有输入先校验；任一写入失败时入口不会启动 HTTP 服务，下次启动可重新生成。
func bootstrapPAASFiles(root string, env paasEnvironment) error {
	template, err := os.ReadFile(filepath.Join(root, "config.example.json"))
	if err != nil {
		return fmt.Errorf("读取镜像配置模板失败: %w", err)
	}
	cfg, err := buildPAASConfig(template, env)
	if err != nil {
		return err
	}
	models, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		return fmt.Errorf("读取镜像模型清单失败: %w", err)
	}
	if !json.Valid(models) {
		return errors.New("镜像模型清单不是有效 JSON")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return errors.New("序列化启动配置失败")
	}
	for _, name := range []string{"config", "logs", "assets"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			return fmt.Errorf("创建运行目录失败: %w", err)
		}
	}
	files := []struct {
		name string
		data []byte
	}{
		{"config.json", append(data, '\n')},
		{"api_keys.txt", []byte("paas:" + env.APIKey + ":environment bootstrap\n")},
		{"models.json", models},
	}
	for _, file := range files {
		if err := writeBootstrapFile(filepath.Join(root, "config", file.name), file.data); err != nil {
			return fmt.Errorf("写入启动文件 %s 失败: %w", file.name, err)
		}
	}
	return nil
}

// 同目录临时文件 + rename，文件权限由 CreateTemp 固定为 0600。
func writeBootstrapFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bootstrap-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
