package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")

	logger := NewDailyLogger(logDir)
	defer func() {
		if logger.latestFd != nil {
			_ = logger.latestFd.Close()
		}
	}()

	// Write something
	msg := []byte("hello world\n")
	n, err := logger.Write(msg)
	if err != nil {
		t.Fatalf("Failed to write to logger: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Expected to write %d bytes, wrote %d", len(msg), n)
	}

	// Verify file is created
	latestPath := filepath.Join(logDir, "logs_latest.log")
	content, err := os.ReadFile(latestPath)
	if err != nil {
		t.Fatalf("Failed to read logs_latest.log: %v", err)
	}
	if !strings.Contains(string(content), string(msg)) {
		t.Fatalf("Expected content to contain %q, got %q", string(msg), string(content))
	}

	// Close to trigger merge
	_ = logger.Close()

	// Verify logs_latest.log is deleted (os.IsNotExist)
	_, err = os.ReadFile(latestPath)
	if err == nil {
		t.Fatalf("Expected logs_latest.log to be deleted")
	}

	nowDate := time.Now().Format("2006-01-02")
	expectedName := fmt.Sprintf("vproxy-%s.log", nowDate)

	// Read merged content
	mergedContent, err := os.ReadFile(filepath.Join(logDir, expectedName))
	if err != nil {
		t.Fatalf("Failed to read merged log file: %v", err)
	}
	if !strings.Contains(string(mergedContent), string(msg)) {
		t.Fatalf("Expected merged content to contain %q, got %q", string(msg), string(mergedContent))
	}

	// Create some old files to test cleanup
	oldDate1 := time.Now().Add(-8 * 24 * time.Hour)
	oldFile1 := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", oldDate1.Format("2006-01-02")))
	if err := os.WriteFile(oldFile1, []byte("old log 1"), 0644); err != nil {
		t.Fatalf("Failed to create old log file: %v", err)
	}
	_ = os.Chtimes(oldFile1, oldDate1, oldDate1) // Ensure mod time is old

	oldDate2 := time.Now().Add(-6 * 24 * time.Hour) // This one should NOT be deleted (within 7 days)
	oldFile2 := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", oldDate2.Format("2006-01-02")))
	if err := os.WriteFile(oldFile2, []byte("old log 2"), 0644); err != nil {
		t.Fatalf("Failed to create old log file: %v", err)
	}
	_ = os.Chtimes(oldFile2, oldDate2, oldDate2)

	// Call cleanup manually
	logger.cleanup()

	// Verify cleanup
	entries, _ := os.ReadDir(logDir)
	var filenames []string
	for _, e := range entries {
		filenames = append(filenames, e.Name())
	}

	// oldFile1 should be deleted, oldFile2 and current file should remain.
	foundOld1 := false
	foundOld2 := false
	for _, name := range filenames {
		if name == filepath.Base(oldFile1) {
			foundOld1 = true
		}
		if name == filepath.Base(oldFile2) {
			foundOld2 = true
		}
	}

	if foundOld1 {
		t.Fatalf("Cleanup failed to delete old file: %s", oldFile1)
	}
	if !foundOld2 {
		t.Fatalf("Cleanup incorrectly deleted recent file: %s", oldFile2)
	}
}

// TestDailyLoggerClear 验证 Clear 清空既有内容、写入标记行，且清空后的写入从文件头继续。
func TestDailyLoggerClear(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	l := NewDailyLogger(logDir)
	defer func() { _ = l.Close() }()

	if _, err := l.Write([]byte("hello log line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	latest := filepath.Join(logDir, "logs_latest.log")
	if b, err := os.ReadFile(latest); err != nil || !strings.Contains(string(b), "hello log line") {
		t.Fatalf("写入后应含日志内容: err=%v content=%q", err, string(b))
	}

	if err := l.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	b, err := os.ReadFile(latest)
	if err != nil {
		t.Fatalf("ReadFile after clear: %v", err)
	}
	if strings.Contains(string(b), "hello log line") {
		t.Fatalf("清空后不应残留旧内容: %q", string(b))
	}
	if !strings.Contains(string(b), "LOG CLEARED") {
		t.Fatalf("清空后应写入标记行: %q", string(b))
	}

	// O_APPEND + Truncate(0)：后续写入应从偏移 0 继续，内容不丢失。
	if _, err := l.Write([]byte("after clear\n")); err != nil {
		t.Fatalf("Write after clear: %v", err)
	}
	b2, _ := os.ReadFile(latest)
	if !strings.Contains(string(b2), "after clear") {
		t.Fatalf("清空后写入应保留: %q", string(b2))
	}
}
