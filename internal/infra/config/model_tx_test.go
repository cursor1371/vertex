package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// modelTxFixture 建立双文件事务测试环境：三个配置文件指向临时目录并清空缓存。
func modelTxFixture(t *testing.T) string {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"),
		`{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`)
	writeRawFile(t, filepath.Join(dir, "model_settings.json"),
		`{"disabled_models":["gemini-old"],"custom_models":[],"model_options":{}}`)
	InvalidateModelsCache()
	InvalidateModelSettingsCache()
	return dir
}

// txFiles 返回目录下事务文件（.txnew/.txold/标记）的存在情况。
func txFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, name := range []string{
		"models.json.txnew", "models.json.txold",
		"model_settings.json.txnew", "model_settings.json.txold", txMarkerName,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		out[name] = err == nil
	}
	return out
}

func assertTxClean(t *testing.T, dir string) {
	t.Helper()
	for name, exists := range txFiles(t, dir) {
		if exists {
			t.Errorf("事务结束后不应残留 %s", name)
		}
	}
}

func txNewContent(t *testing.T) ([]string, map[string]string, map[string]string, ModelSettings) {
	t.Helper()
	return []string{"gemini-3.6-flash", "gemini-3.7-flash"},
		map[string]string{"别名": "gemini-3.7-flash"},
		map[string]string{"gemini-3.7-flash": FamilyNameText},
		ModelSettings{
			DisabledModels: []string{"gemini-old"},
			CustomModels:   []string{"gemini-3.7-flash"},
			ModelOptions:   map[string]ModelOptions{"gemini-3.7-flash": {TrailingTurnFix: optBool(true)}},
		}
}

// TestSaveModelConfigTx_Success 成功路径：两份文件都换新、.bak 保留旧内容、
// 事务文件清理干净、缓存失效（读回即新值）。
func TestSaveModelConfigTx_Success(t *testing.T) {
	dir := modelTxFixture(t)
	modelsBefore, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	settingsBefore, _ := os.ReadFile(filepath.Join(dir, "model_settings.json"))

	models, alias, family, settings := txNewContent(t)
	if err := SaveModelConfigTx(models, alias, family, settings); err != nil {
		t.Fatalf("SaveModelConfigTx: %v", err)
	}

	// 读回即新值（缓存已失效）
	if got := BaseModels(); len(got) != 2 || got[1] != "gemini-3.7-flash" {
		t.Fatalf("BaseModels=%v", got)
	}
	if got := DisabledModels(); len(got) != 1 || got[0] != "gemini-old" {
		t.Fatalf("DisabledModels=%v", got)
	}
	if !TrailingTurnFixEnabled("gemini-3.7-flash") {
		t.Fatal("模型级开关未生效")
	}

	// .bak 保留事务前内容（用户可恢复）
	modelsBak, err := os.ReadFile(filepath.Join(dir, "models.json.bak"))
	if err != nil {
		t.Fatalf("models.json.bak 应存在: %v", err)
	}
	if !bytes.Equal(modelsBak, modelsBefore) {
		t.Fatal("models.json.bak 应等于事务前内容")
	}
	settingsBak, err := os.ReadFile(filepath.Join(dir, "model_settings.json.bak"))
	if err != nil {
		t.Fatalf("model_settings.json.bak 应存在: %v", err)
	}
	if !bytes.Equal(settingsBak, settingsBefore) {
		t.Fatal("model_settings.json.bak 应等于事务前内容")
	}
	assertTxClean(t, dir)
}

// TestSaveModelConfigTx_StagingFailure 暂存写入失败：磁盘逐字节保持原状、无残留事务文件。
func TestSaveModelConfigTx_StagingFailure(t *testing.T) {
	dir := modelTxFixture(t)
	modelsBefore, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	settingsBefore, _ := os.ReadFile(filepath.Join(dir, "model_settings.json"))

	// 故障注入：暂存路径被目录占据 → 写入失败（跨平台通用）
	if err := os.MkdirAll(filepath.Join(dir, "model_settings.json.txnew"), 0o755); err != nil {
		t.Fatal(err)
	}
	models, alias, family, settings := txNewContent(t)
	if err := SaveModelConfigTx(models, alias, family, settings); err == nil {
		t.Fatal("暂存写入失败应返回错误")
	}
	assertBytes(t, filepath.Join(dir, "models.json"), modelsBefore)
	assertBytes(t, filepath.Join(dir, "model_settings.json"), settingsBefore)
}

// TestSaveModelConfigTx_BackupFailure 备份失败：事务中止在替换之前，磁盘保持全旧。
func TestSaveModelConfigTx_BackupFailure(t *testing.T) {
	dir := modelTxFixture(t)
	modelsBefore, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	settingsBefore, _ := os.ReadFile(filepath.Join(dir, "model_settings.json"))

	// 故障注入：models.json.bak 被目录占据 → 备份写入失败
	if err := os.MkdirAll(filepath.Join(dir, "models.json.bak"), 0o755); err != nil {
		t.Fatal(err)
	}
	models, alias, family, settings := txNewContent(t)
	if err := SaveModelConfigTx(models, alias, family, settings); err == nil {
		t.Fatal("备份失败应返回错误")
	}
	assertBytes(t, filepath.Join(dir, "models.json"), modelsBefore)
	assertBytes(t, filepath.Join(dir, "model_settings.json"), settingsBefore)
	// 暂存与标记必须清理（.txnew 目录是注入物，不算残留）
	if tx := txFiles(t, dir); tx[txMarkerName] || tx["models.json.txold"] || tx["model_settings.json.txold"] {
		t.Fatalf("不应残留事务文件: %v", tx)
	}
}

// TestSaveModelConfigTx_SecondRenameFailsRollsBack 替换 #2 失败：回滚 models.json 为全旧，
// 缓存不失效（读回仍是旧值），事务文件清理干净。
func TestSaveModelConfigTx_SecondRenameFailsRollsBack(t *testing.T) {
	dir := modelTxFixture(t)
	modelsBefore, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	settingsBefore, _ := os.ReadFile(filepath.Join(dir, "model_settings.json"))

	settingsTarget := filepath.Join(dir, "model_settings.json")
	prevRename := renameModelFile
	renameModelFile = func(oldpath, newpath string) error {
		if newpath == settingsTarget {
			return errors.New("注入：替换 model_settings.json 失败")
		}
		return prevRename(oldpath, newpath)
	}
	t.Cleanup(func() { renameModelFile = prevRename })

	models, alias, family, settings := txNewContent(t)
	if err := SaveModelConfigTx(models, alias, family, settings); err == nil {
		t.Fatal("替换 #2 失败应返回错误")
	}
	assertBytes(t, filepath.Join(dir, "models.json"), modelsBefore)
	assertBytes(t, filepath.Join(dir, "model_settings.json"), settingsBefore)
	assertTxClean(t, dir)
	// 缓存未被失效：读回仍是旧值
	if got := BaseModels(); len(got) != 1 || got[0] != "gemini-3.6-flash" {
		t.Fatalf("回滚后读回应为旧值: %v", got)
	}
}

// TestSaveModelConfigTx_RollbackFailsKeepsMarkerThenRecovers 回滚也失败：
// 保留标记与暂存，启动前滚收敛到"两侧全新"。
func TestSaveModelConfigTx_RollbackFailsKeepsMarkerThenRecovers(t *testing.T) {
	dir := modelTxFixture(t)

	settingsTarget := filepath.Join(dir, "model_settings.json")
	modelsTarget := filepath.Join(dir, "models.json")
	prevRename := renameModelFile
	// 有状态注入：替换 #1（models）成功；替换 #2（settings）失败，且随后对 models 的
	// 回滚也失败——精确模拟"回滚失败、必须靠启动前滚收敛"的场景。
	settingsRenameFailed := false
	renameModelFile = func(oldpath, newpath string) error {
		if newpath == settingsTarget {
			settingsRenameFailed = true
			return errors.New("注入：替换 model_settings.json 失败")
		}
		if newpath == modelsTarget && settingsRenameFailed {
			return errors.New("注入：回滚 models.json 失败")
		}
		return prevRename(oldpath, newpath)
	}
	t.Cleanup(func() { renameModelFile = prevRename })

	models, alias, family, settings := txNewContent(t)
	if err := SaveModelConfigTx(models, alias, family, settings); err == nil {
		t.Fatal("应返回错误")
	}
	// 标记与暂存保留（交给启动前滚）
	tx := txFiles(t, dir)
	if !tx[txMarkerName] || !tx["model_settings.json.txnew"] {
		t.Fatalf("回滚失败时应保留标记与暂存: %v", tx)
	}

	// 解除注入 → 启动前滚 → 两侧一致为最新内容
	renameModelFile = prevRename
	InvalidateModelsCache()
	InvalidateModelSettingsCache()
	RecoverModelConfigTx()

	if got := BaseModels(); len(got) != 2 || got[1] != "gemini-3.7-flash" {
		t.Fatalf("前滚后 models.json 应为最新内容: %v", got)
	}
	if got := CustomModels(); len(got) != 1 || got[0] != "gemini-3.7-flash" {
		t.Fatalf("前滚后 model_settings.json 应为最新内容: %v", got)
	}
	assertTxClean(t, dir)

	// 幂等：再次前滚无副作用
	before := snapshotFiles(t, dir, "models.json", "model_settings.json")
	RecoverModelConfigTx()
	after := snapshotFiles(t, dir, "models.json", "model_settings.json")
	for name, b := range before {
		if !bytes.Equal(b, after[name]) {
			t.Fatalf("重复前滚应无副作用: %s 被改写", name)
		}
	}
}

// TestRecoverModelConfigTx_CrashBetweenRenames 模拟"恰在两次替换之间崩溃"：
// 标记 + 未完成的暂存存在时，启动前滚把两文件收敛为全新。
func TestRecoverModelConfigTx_CrashBetweenRenames(t *testing.T) {
	dir := modelTxFixture(t)

	// 构造崩溃态：models.json 已是新内容，settings 仍旧，settings.txnew 待替换
	writeRawFile(t, filepath.Join(dir, "models.json"),
		`{"models":["gemini-3.6-flash","gemini-3.7-flash"],"alias_map":{},"family_map":{}}`)
	writeRawFile(t, filepath.Join(dir, "model_settings.json.txnew"),
		`{"disabled_models":[],"custom_models":["gemini-3.7-flash"],"model_options":{}}`)
	writeRawFile(t, filepath.Join(dir, txMarkerName), `{"stage":"commit","started_at":"2026-10-14T00:00:00Z"}`)
	InvalidateModelsCache()
	InvalidateModelSettingsCache()

	RecoverModelConfigTx()

	if got := CustomModels(); len(got) != 1 || got[0] != "gemini-3.7-flash" {
		t.Fatalf("前滚应完成 settings 替换: %v", got)
	}
	assertTxClean(t, dir)
}

// TestRecoverModelConfigTx_NoMarkerIsNoop 无标记时前滚为空操作（普通启动零开销）。
func TestRecoverModelConfigTx_NoMarkerIsNoop(t *testing.T) {
	dir := modelTxFixture(t)
	before := snapshotFiles(t, dir, "models.json", "model_settings.json")
	RecoverModelConfigTx()
	after := snapshotFiles(t, dir, "models.json", "model_settings.json")
	for name, b := range before {
		if !bytes.Equal(b, after[name]) {
			t.Fatalf("无标记不应改写 %s", name)
		}
	}
	assertTxClean(t, dir)
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s 应逐字节保持原状:\n got=%s\nwant=%s", path, got, want)
	}
}
