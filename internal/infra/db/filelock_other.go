//go:build !windows

package db

// FileInUse 报告 path 是否被其他进程占用。
//
// 非 Windows 平台没有等价的"零共享模式"打开语义，这里恒返回 false：
// 压缩流程后续的 rename 若真的撞上占用会直接失败并报错，不会造成损坏，
// 只是把探测推迟到了替换那一步。
func FileInUse(path string) bool {
	_ = path
	return false
}
