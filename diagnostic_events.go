package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

// The operator supplies a private, read-only profile and sibling salt file.
// A bad diagnostic input disables the side channel; it cannot change run errors.
func openDiagnostics(path string, output io.Writer) *scaleset.Diagnostics {
	if path == "" {
		return nil
	}
	owner := uint32(os.Geteuid())
	if value := os.Getenv("CI_DIAGNOSTIC_OWNER_UID"); value != "" {
		n, e := strconv.ParseUint(value, 10, 32)
		if e != nil {
			return nil
		}
		owner = uint32(n)
	}
	read := func(name string, max int64) ([]byte, bool) {
		fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, false
		}
		f := os.NewFile(uintptr(fd), "diagnostic input")
		defer f.Close()
		s, err := f.Stat()
		if err != nil || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > max {
			return nil, false
		}
		st, ok := s.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != owner || st.Nlink != 1 {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(f, max+1))
		return b, err == nil && int64(len(b)) <= max
	}
	b, ok := read(path, 1024)
	if !ok {
		return nil
	}
	var id scaleset.DiagnosticIdentity
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&id) != nil || dec.Decode(new(any)) != io.EOF || id.SourceRevision != imageRevision {
		return nil
	}
	saltBytes, ok := read(filepath.Join(filepath.Dir(path), "salt"), 32)
	if !ok || len(saltBytes) != 32 {
		return nil
	}
	var salt [32]byte
	copy(salt[:], saltBytes)
	d, err := scaleset.NewDiagnostics(id, salt, output, 512)
	if err != nil {
		return nil
	}
	return d
}
func closeDiagnostics(d *scaleset.Diagnostics) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Close(ctx)
}
