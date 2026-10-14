//go:build windows

package diskfree

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// of 取 Windows 卷的可用/总空间。
func of(path string) (Space, error) {
	p, err := windows.UTF16PtrFromString(dirOf(path))
	if err != nil {
		return Space{}, fmt.Errorf("error: %w", err)
	}
	var freeAvail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree); err != nil {
		return Space{}, fmt.Errorf("error: %w", err)
	}
	return Space{Free: int64(freeAvail), Total: int64(total)}, nil
}
