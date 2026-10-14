package api

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// APIKeyManager 简化版 API 密钥管理器。
type APIKeyManager struct { //nolint:govet
	mu       sync.RWMutex
	keys     map[string]string // api_key -> name
	keysFile string
}

// NewAPIKeyManager 构造管理器（密钥文件路径同 config 的解析策略）。
func NewAPIKeyManager() *APIKeyManager {
	return &APIKeyManager{keys: map[string]string{}, keysFile: keysFilePath()} //nolint:exhaustruct
}

func keysFilePath() string {
	if p := os.Getenv("VPROXY_API_KEYS"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "api_keys.txt")
		if _, errStat := os.Stat(p); errStat == nil { //nolint:govet
			return p
		}
	}
	return filepath.Join("config", "api_keys.txt")
}

// LoadKeys 从 config/api_keys.txt 加载密钥。
// 格式：name:api_key:description（每行），api_key 必须以 "sk-" 开头，否则跳过。
func (m *APIKeyManager) LoadKeys() bool {
	f, err := os.Open(m.keysFile)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadKeysUnlocked(f)
}

// loadKeysUnlocked 解析已打开的密钥文件并重建内存表。调用方必须已持有 m.mu。
func (m *APIKeyManager) loadKeysUnlocked(f *os.File) bool {
	m.keys = map[string]string{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		key := strings.TrimSpace(parts[1])
		if !strings.HasPrefix(key, "sk-") {
			continue
		}
		m.keys[key] = name
	}
	if errScan := sc.Err(); errScan != nil { //nolint:govet
		return false
	}
	return true
}

// ValidateKey 校验密钥是否有效。
func (m *APIKeyManager) ValidateKey(key string) bool {
	if key == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.keys[strings.TrimSpace(key)]
	return ok
}

// NameOf 返回密钥对应的名称；未找到返回 (false)。
func (m *APIKeyManager) NameOf(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, ok := m.keys[strings.TrimSpace(key)]
	return name, ok
}

// Count 返回已加载的密钥数。
func (m *APIKeyManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// extractAPIKey 从请求提取 API key：
// Bearer 头 > x-goog-api-key 头 > query 参数 key。
func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" && strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if g := r.Header.Get("x-goog-api-key"); g != "" {
		return strings.TrimSpace(g)
	}
	if k := r.URL.Query().Get("key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// ---- admin 后台的密钥读写 ----
//
// 这些方法直接操作 api_keys.txt 文件（保留每行的 name:key:description 三段格式与顺序），
// 而非改内存 map——文件是真相之源，改完调 LoadKeys 重载内存。

// apiKeyEntry 是密钥文件里的一行（name、key、可选描述）。
type apiKeyEntry struct {
	Name        string
	Key         string
	Description string
}

// readEntries 解析 api_keys.txt 为有序条目列表（跳过空行/注释/字段不足的行）。
func (m *APIKeyManager) readEntries() ([]apiKeyEntry, error) {
	f, err := os.Open(m.keysFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 文件不存在视为空列表
		}
		return nil, fmt.Errorf("error: %w", err)

	}
	defer func() { _ = f.Close() }()

	var out []apiKeyEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		e := apiKeyEntry{Name: strings.TrimSpace(parts[0]), Key: strings.TrimSpace(parts[1])} //nolint:exhaustruct
		if len(parts) >= 3 {
			e.Description = strings.TrimSpace(parts[2])
		}
		out = append(out, e)
	}
	return out, sc.Err() //nolint:wrapcheck
}

// writeEntries 原子写回 api_keys.txt（先写 .tmp 再 rename），保留三段格式与表头。
func (m *APIKeyManager) writeEntries(entries []apiKeyEntry) error {
	if dir := filepath.Dir(m.keysFile); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("error: %w", err)

		}
	}
	var b strings.Builder
	b.WriteString("# 格式: name:key:description （由管理面板维护）\n")
	for _, e := range entries {
		if e.Name == "" || e.Key == "" {
			continue
		}
		b.WriteString(e.Name)
		b.WriteByte(':')
		b.WriteString(e.Key)
		if e.Description != "" {
			b.WriteByte(':')
			b.WriteString(e.Description)
		}
		b.WriteByte('\n')
	}
	// 唯一临时名：固定 ".tmp" 会在并发写回时互相覆盖后再 rename，导致密钥丢失/复活。
	// CreateTemp 以 0o600 创建，权限语义与原实现一致。
	tmp, err := os.CreateTemp(filepath.Dir(m.keysFile), ".api_keys-*.tmp")
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // rename 成功后此路径已不存在，Remove 报错无害
	}()
	if _, err = tmp.WriteString(b.String()); err != nil { //nolint:govet
		return fmt.Errorf("error: %w", err)
	}
	if err = tmp.Close(); err != nil { //nolint:govet
		return fmt.Errorf("error: %w", err)
	}
	return os.Rename(tmpName, m.keysFile) //nolint:wrapcheck
}

// List 返回当前所有密钥条目（供 admin 列表展示；调用方负责脱敏，勿直接回明文 key）。
func (m *APIKeyManager) List() ([]apiKeyEntry, error) {
	return m.readEntries()
}

// Add 新增（或按 name 覆盖）一个密钥，写回文件并重载内存。
// 先剔除同名旧条目，再追加，最后 load_keys。
//
// 全程持有 m.mu：Add/Delete 是"读文件-改-写回"的临界区，不持锁时两个并发写回
// 各持同一基线互相覆盖，被删密钥会"复活"；写盘失败必须如实上抛（此前误返回
// 读文件阶段的 nil，面板显示成功但磁盘未变）。
func (m *APIKeyManager) Add(name, key, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.readEntries()
	if err != nil {
		return err
	}
	kept := entries[:0:0] // 新切片，不复用底层数组
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	kept = append(kept, apiKeyEntry{Name: name, Key: key, Description: description})
	if errW := m.writeEntries(kept); errW != nil {
		return errW
	}
	m.reloadKeysUnlocked()
	return nil
}

// Delete 按 name 删除一个密钥，写回文件并重载内存。返回 false 表示未找到该名称。
// 未找到时调用方返回 404。加锁语义同 Add。
func (m *APIKeyManager) Delete(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.readEntries()
	if err != nil {
		return false, err
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return false, nil // 没删掉任何条目
	}
	if errW := m.writeEntries(kept); errW != nil {
		return false, errW
	}
	m.reloadKeysUnlocked()
	return true, nil
}

// reloadKeysUnlocked 在已持有 m.mu 的前提下重读密钥文件刷新内存表（best-effort，
// 与原 Add/Delete 末尾忽略 LoadKeys 返回值的语义一致）。
func (m *APIKeyManager) reloadKeysUnlocked() {
	f, err := os.Open(m.keysFile)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_ = m.loadKeysUnlocked(f)
}
