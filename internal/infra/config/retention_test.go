package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestRaceLogRetentionDays 验证 race_log 独立保留期：默认 7 天，非法值回落 7（而非 0）。
// 下界必须是 1：0 会让清理 cutoff 落到"现在"，一次删光全部历史。
func TestRaceLogRetentionDays(t *testing.T) {
	t.Run("默认 7 天", func(t *testing.T) {
		if got := DefaultConfig().RaceLogRetentionDays; got != 7 {
			t.Fatalf("默认 race_log_retention_days = %d, want 7", got)
		}
	})

	t.Run("非法值回落 7", func(t *testing.T) {
		for _, v := range []int{0, -1} {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			t.Setenv("VPROXY_CONFIG", path)
			InvalidateCache()

			if err := os.WriteFile(path, []byte(`{"race_log_retention_days":`+strconv.Itoa(v)+`}`), 0o644); err != nil {
				t.Fatal(err)
			}
			InvalidateCache()
			if got := Load().RaceLogRetentionDays; got != 7 {
				t.Errorf("race_log_retention_days=%d 应回落 7, got %d", v, got)
			}
		}
	})

	t.Run("合法值透传", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		t.Setenv("VPROXY_CONFIG", path)
		InvalidateCache()

		if err := os.WriteFile(path, []byte(`{"race_log_retention_days":30}`), 0o644); err != nil {
			t.Fatal(err)
		}
		InvalidateCache()
		if got := Load().RaceLogRetentionDays; got != 30 {
			t.Fatalf("race_log_retention_days = %d, want 30", got)
		}
	})

	InvalidateCache() // 清理，避免污染其它用例
}
