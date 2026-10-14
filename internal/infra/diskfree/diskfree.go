// Package diskfree 提供跨平台的磁盘可用空间查询。
//
// 仅服务于数据库维护（判断能否安全执行完整 VACUUM、在 admin 面板展示磁盘余量），
// 故只暴露最小 API。各平台实现分文件：windows 走 GetDiskFreeSpaceEx，
// 其余（linux/android/darwin）走 statfs。
package diskfree

import (
	"os"
	"path/filepath"
)

// Space 是某个挂载点的空间快照（字节）。
type Space struct {
	Free  int64 // 调用方可用的空闲字节
	Total int64 // 该挂载点总容量
}

// Of 返回给定路径所在挂载点的空间快照。path 可以是文件或目录，
// 各平台实现自行取其所在卷/挂载点。
func Of(path string) (Space, error) {
	return of(path)
}

// dirOf 把可能指向文件的 path 归一为其所在目录。
// 查询类接口（如 Windows 的 GetDiskFreeSpaceEx）对文件路径会失败，须先归一。
func dirOf(path string) string {
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return filepath.Dir(path)
	}
	return path
}
