// 本文件实现上游目录快照（model_catalog.json）与查询签名缓存（model_catalog_sig.txt）的读写。
//
// 两个文件都与 models.json 同目录：
//   - model_catalog.json：最近一次**成功**拉取的上游目录快照、最近一次失败原因与
//     人工确认时间。快照是"上游目录（可发现）"概念的持久化载体，与本地模型配置完全分离：
//     获取上游目录只写本文件，绝不改动 models.json / model_settings.json。
//   - model_catalog_sig.txt：ListPublisherModel 的查询签名缓存，与 scripts/更新模型.bat 共用。
//
// 读取路径不做内存缓存：仅在管理面板拉取与展示时访问，文件很小，读盘成本可忽略。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CatalogSnapshot 是上游目录快照。
//
// 完整性语义：Complete=true 且 LastError 为空时，Models 是"可信集合"，
// 据此计算出的「已移除模型」才是可执行的；任一条不满足（最近一次拉取失败、
// 或快照为异常骤降后待确认状态）时只能提示、不得执行移除。
type CatalogSnapshot struct {
	FetchedAt    string   `json:"fetched_at"`             // 最近一次成功拉取时间（RFC3339）
	Source       string   `json:"source"`                 // 目录来源（publishers/google）
	Signature    string   `json:"signature,omitempty"`    // 本次使用的查询签名（审计）
	ConfirmedAt  string   `json:"confirmed_at,omitempty"` // 数量骤降被管理员显式确认接受的时间
	LastError    string   `json:"last_error,omitempty"`   // 最近一次拉取失败原因（不覆盖 Models）
	Models       []string `json:"models"`                 // 过滤后的候选（可信集合）
	Pages        int      `json:"pages"`                  // 实际翻页次数
	SkippedCount int      `json:"skipped_count"`          // 被过滤掉的条数
	Complete     bool     `json:"complete"`               // 是否完整分页成功
}

// CatalogSnapshotPath 返回快照文件路径（与 models.json 同目录）。
func CatalogSnapshotPath() string {
	if p := os.Getenv("VPROXY_MODEL_CATALOG"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(modelsPath()), "model_catalog.json")
}

// CatalogSignaturePath 返回签名缓存文件路径（与 models.json 同目录，与更新模型.bat 共用）。
func CatalogSignaturePath() string {
	return filepath.Join(filepath.Dir(modelsPath()), "model_catalog_sig.txt")
}

// LoadCatalogSnapshot 读取快照；文件不存在返回 (nil, nil)。
func LoadCatalogSnapshot() (*CatalogSnapshot, error) {
	data, err := os.ReadFile(CatalogSnapshotPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error: %w", err)
	}
	var snap CatalogSnapshot
	if uerr := json.Unmarshal(data, &snap); uerr != nil {
		return nil, fmt.Errorf("error: %w", uerr)
	}
	return &snap, nil
}

// SaveCatalogSnapshot 原子写入快照（写前备份 .bak，与其它配置文件一致）。
func SaveCatalogSnapshot(snap *CatalogSnapshot) error {
	if snap == nil {
		return fmt.Errorf("error: nil snapshot")
	}
	if err := backupFileIfExists(CatalogSnapshotPath()); err != nil {
		return err
	}
	return writeJSONFile(CatalogSnapshotPath(), snap)
}

// LoadCatalogSignature 读取缓存的查询签名；不存在或为空返回空串。
func LoadCatalogSignature() string {
	data, err := os.ReadFile(CatalogSignaturePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// SaveCatalogSignature 写入查询签名缓存（原子替换；失败仅返回错误，由调用方决定是否忽略）。
func SaveCatalogSignature(sig string) error {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return fmt.Errorf("error: empty signature")
	}
	tmp := CatalogSignaturePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(sig), 0o644); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return os.Rename(tmp, CatalogSignaturePath()) //nolint:wrapcheck
}
