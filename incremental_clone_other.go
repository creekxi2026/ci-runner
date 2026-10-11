//go:build !linux

package main

import "os"

func cloneCacheFile(src, dst *os.File) (bool, error) { return false, nil }
