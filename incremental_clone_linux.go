//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func cloneCacheFile(src, dst *os.File) (bool, error) {
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), 0x40049409, src.Fd())
	if err == 0 {
		return true, nil
	}
	for _, fallback := range []error{syscall.EXDEV, syscall.EOPNOTSUPP, syscall.ENOTTY, syscall.EINVAL} {
		if errors.Is(err, fallback) {
			return false, nil
		}
	}
	return false, err
}
