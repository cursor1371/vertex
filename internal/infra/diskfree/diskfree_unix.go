//go:build !windows

package diskfree

import (
	"fmt"
	"syscall"
)

// of 取 unix 系（linux / android / darwin）挂载点的可用/总空间。
// Bsize 在 darwin 是 uint32、linux 是 int64，统一转 uint64 后相乘。
func of(path string) (Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dirOf(path), &st); err != nil {
		return Space{}, fmt.Errorf("error: %w", err)
	}
	bsize := uint64(st.Bsize)
	return Space{
		Free:  int64(bsize * st.Bavail),
		Total: int64(bsize * st.Blocks),
	}, nil
}
