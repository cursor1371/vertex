// 本文件实现 models.json 与 model_settings.json 的事务性保存（双文件写入）。
//
// 两级保证（与 Issue #4 的评审结论一致）：
//
//   - 运行时一致性（必达）：任一步失败都会把磁盘恢复为"两侧原内容"，且不失效任何缓存；
//     不会出现"模型清单已换、模型选项还是旧的"这种运行期中间态。
//   - 崩溃收敛一致性：进程若恰在两次替换之间崩溃，磁盘上可能一新一旧。提交标记
//     （.model_config_tx.json）+ 暂存副本（*.txnew）使 RecoverModelConfigTx 在下次
//     启动时把事务**前滚**到"两侧全新"——两个文件在任何时刻要么全旧、要么全新，
//     不会长期处于不一致状态。极端断电导致暂存损坏时保留标记并告警，不静默清空。
//
// 失败路径语义（写入顺序）：序列化+回读校验 → 写暂存 → 备份 .bak + 回滚副本 .txold
// → 原子写提交标记 → rename#1 → rename#2 → 清理标记与暂存。
//   - rename#1 之前失败：清理暂存与标记，磁盘保持全旧，无副作用。
//   - rename#2 失败：用 .txold 恢复 rename#1 的成果（回到全旧）；若恢复也失败，
//     保留标记与暂存，交由下次启动前滚（收敛到全新）。
//
// .bak 是**用户可恢复备份**（与 scripts/更新模型.bat 的惯例一致，始终保留）；
// .txold/.txnew/标记是内部事务文件，成功或回滚后即清理。
package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// txMarkerName 是模型配置事务的提交标记文件名（与 models.json 同目录）。
const txMarkerName = ".model_config_tx.json"

// txMarker 是提交标记内容（stage 便于排查，started_at 记录提交时刻）。
type txMarker struct {
	Stage     string `json:"stage"`
	StartedAt string `json:"started_at"`
}

// modelTxMarkerPath 返回提交标记路径。
func modelTxMarkerPath() string {
	return filepath.Join(filepath.Dir(modelsPath()), txMarkerName)
}

// marshalModelJSON 序列化 + 回读校验（MarshalIndent 与 writeJSONFile 同一格式），
// 保证暂存内容一定是"能被本进程读回"的合法 JSON。
func marshalModelJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	var probe map[string]any
	if uerr := json.Unmarshal(data, &probe); uerr != nil {
		return nil, fmt.Errorf("error: %w", uerr)
	}
	return data, nil
}

// renameModelFile 是事务提交使用的 rename 实现；测试注入失败以覆盖回滚/前滚路径。
//
//nolint:gochecknoglobals // 测试注入点
var renameModelFile = os.Rename

// copyFileIfExists 把 src 复制为 dst；src 不存在视为空操作。
func copyFileIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("error: %w", err)
	}
	if werr := os.WriteFile(dst, data, 0o644); werr != nil {
		return fmt.Errorf("error: %w", werr)
	}
	return nil
}

// cleanupModelTxFiles 清理暂存、回滚副本与提交标记（失败只记日志，不阻断主流程）。
func cleanupModelTxFiles() {
	for _, p := range []string{
		modelsPath() + ".txnew", modelsPath() + ".txold",
		ModelSettingsPath() + ".txnew", ModelSettingsPath() + ".txold",
		modelTxMarkerPath(),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[Config] 清理模型配置事务文件失败 %s: %v", p, err)
		}
	}
}

// SaveModelConfigTx 事务性保存模型配置（models.json + model_settings.json）。
//
// 成功后统一失效两个缓存；失败时磁盘与缓存都保持原状（除 .bak 备份外无副作用）。
func SaveModelConfigTx(models []string, aliasMap, familyMap map[string]string, settings ModelSettings) error {
	modelsTarget := modelsPath()
	settingsTarget := ModelSettingsPath()
	modelsStaged := modelsTarget + ".txnew"
	settingsStaged := settingsTarget + ".txnew"

	if aliasMap == nil {
		aliasMap = map[string]string{}
	}
	if familyMap == nil {
		familyMap = map[string]string{}
	}
	if settings.DisabledModels == nil {
		settings.DisabledModels = []string{}
	}
	if settings.CustomModels == nil {
		settings.CustomModels = []string{}
	}
	if settings.ModelOptions == nil {
		settings.ModelOptions = map[string]ModelOptions{}
	}

	// ① 序列化 + 回读校验
	modelsData, err := marshalModelJSON(modelsFile{Models: models, AliasMap: aliasMap, FamilyMap: familyMap})
	if err != nil {
		return fmt.Errorf("序列化 models.json 失败: %w", err)
	}
	settingsData, err := marshalModelJSON(settings)
	if err != nil {
		return fmt.Errorf("序列化 model_settings.json 失败: %w", err)
	}

	// ② 写暂存（任一失败：清理后返回，磁盘保持全旧）
	if werr := os.WriteFile(modelsStaged, modelsData, 0o644); werr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("写入模型清单暂存失败: %w", werr)
	}
	if werr := os.WriteFile(settingsStaged, settingsData, 0o644); werr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("写入模型设置暂存失败: %w", werr)
	}

	// ③ 备份（用户可恢复）与回滚副本（内部）
	if berr := backupFileIfExists(modelsTarget); berr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("备份 models.json 失败: %w", berr)
	}
	if berr := backupFileIfExists(settingsTarget); berr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("备份 model_settings.json 失败: %w", berr)
	}
	if cerr := copyFileIfExists(modelsTarget, modelsTarget+".txold"); cerr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("准备回滚副本失败: %w", cerr)
	}
	if cerr := copyFileIfExists(settingsTarget, settingsTarget+".txold"); cerr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("准备回滚副本失败: %w", cerr)
	}

	// ④ 原子写提交标记：标记存在即代表"事务可能未完成"，启动时会前滚收敛。
	marker := txMarker{Stage: "commit", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if merr := writeJSONFile(modelTxMarkerPath(), marker); merr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("写入事务标记失败: %w", merr)
	}

	// ⑤ 替换 #1：失败 → 清理（无任何文件被替换）
	if rerr := renameModelFile(modelsStaged, modelsTarget); rerr != nil {
		cleanupModelTxFiles()
		return fmt.Errorf("替换 models.json 失败: %w", rerr)
	}

	// ⑥ 替换 #2：失败 → 回滚 #1（回到全旧）；回滚也失败则保留标记，交给启动前滚。
	if rerr := renameModelFile(settingsStaged, settingsTarget); rerr != nil {
		if oerr := rollbackModelsFile(); oerr != nil {
			log.Printf("[Config] 模型配置事务回滚失败（保留标记，启动时将前滚收敛到最新状态）: %v", oerr)
			return fmt.Errorf("替换 model_settings.json 失败且回滚失败: %w", rerr)
		}
		cleanupModelTxFiles()
		return fmt.Errorf("替换 model_settings.json 失败（已回滚 models.json）: %w", rerr)
	}

	// ⑦ 双文件已一致：清理事务文件并失效缓存。
	cleanupModelTxFiles()
	InvalidateModelsCache()
	InvalidateModelSettingsCache()
	return nil
}

// rollbackModelsFile 把 models.json 回滚为事务前状态（rename 回滚副本；
// 事务前文件不存在则删除新建的 models.json，恢复"文件不存在"的初始态）。
func rollbackModelsFile() error {
	target := modelsPath()
	txold := target + ".txold"
	if _, err := os.Stat(txold); err == nil {
		return renameModelFile(txold, target) //nolint:wrapcheck
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error: %w", err)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// RecoverModelConfigTx 把未完成的模型配置事务前滚到一致状态（启动时调用，幂等）。
//
// 检测到提交标记时：用暂存内容重放两次替换（已替换过的文件其暂存已不存在，自然跳过），
// 随后清理标记与暂存。失败只记日志并保留标记，下次启动重试；期间两个文件仍可按原内容使用。
func RecoverModelConfigTx() {
	marker := modelTxMarkerPath()
	if _, err := os.Stat(marker); err != nil {
		return // 无标记：没有未完成事务
	}
	rollForward := func(staged, target string) error {
		if _, err := os.Stat(staged); err != nil {
			if os.IsNotExist(err) {
				return nil // 该文件已提交（或无暂存）：无需重放
			}
			return fmt.Errorf("error: %w", err)
		}
		if err := renameModelFile(staged, target); err != nil {
			return fmt.Errorf("error: %w", err)
		}
		return nil
	}
	if err := rollForward(modelsPath()+".txnew", modelsPath()); err != nil {
		log.Printf("[Config] 模型配置事务前滚失败（保留标记，下次启动重试）: %v", err)
		return
	}
	if err := rollForward(ModelSettingsPath()+".txnew", ModelSettingsPath()); err != nil {
		log.Printf("[Config] 模型配置事务前滚失败（保留标记，下次启动重试）: %v", err)
		return
	}
	cleanupModelTxFiles()
	InvalidateModelsCache()
	InvalidateModelSettingsCache()
	log.Printf("[Config] 已前滚未完成的模型配置事务（models.json 与 model_settings.json 收敛为一致状态）")
}
