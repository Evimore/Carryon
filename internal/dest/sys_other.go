//go:build !linux

package dest

import "errors"

func sameDevice(string, Mount) bool { return false }

// FreeSpace is only available on Linux.
func FreeSpace(root, path string) (uint64, error) {
	return 0, errors.New("checking free space is only supported on Linux")
}
