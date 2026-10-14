//go:build windows

package db

import "golang.org/x/sys/windows"

// FileInUse 报告 path 是否被其他进程占用（Windows 实现）。
//
// 以 dwShareMode=0 打开探测：只要别的进程（例如正在运行的 vertex-proxy）
// 持有该文件的任何句柄，CreateFile 就会以 ERROR_SHARING_VIOLATION 失败。
// 文件不存在视为未占用（由调用方另行判断是否存在）。
func FileInUse(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		0, // 不共享：任何既存句柄都会让本次打开失败
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			return false
		}
		return true
	}
	_ = windows.CloseHandle(h)
	return false
}
