package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// The shared lock is used only to initialize/migrate the on-disk format.
// Runtime admission and cleanup use the pool's mutex and exclusive owner lock.
func (s *sharedWork) initializeLayout() error {
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	want := sharedWorkSchema + "\n" + s.volume + "\nadmission=per-pool-v1\n"
	old := sharedWorkSchema + "\n" + s.volume + "\nlimit=3\n"
	got, err := s.root.ReadFile("schema")
	if err == nil && string(got) == want {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && string(got) != old {
		return fmt.Errorf("shared work schema/volume mismatch")
	}
	// Old controllers do not understand independent quotas. Require a drained,
	// stopped fleet for migration; never silently discard an outstanding lease.
	migrating := err == nil
	owners, err := s.root.Open("owners")
	if err != nil {
		return err
	}
	entries, err := owners.ReadDir(-1)
	owners.Close()
	if err != nil {
		return err
	}
	var guards []*os.File
	defer func() {
		for _, f := range guards {
			f.Close()
		}
	}()
	for _, entry := range entries {
		if !migrating || entry.Name() == s.owner {
			continue
		}
		f, e := s.root.OpenFile("owners/"+entry.Name(), os.O_RDWR, 0)
		if e != nil {
			return e
		}
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			f.Close()
			return fmt.Errorf("stop all other controllers before admission migration: %w", e)
		}
		guards = append(guards, f)
	}
	for _, dir := range []string{"leases", "jobs"} {
		f, e := s.root.Open(dir)
		if e != nil {
			return e
		}
		entries, e := f.ReadDir(-1)
		f.Close()
		if e != nil {
			return e
		}
		if len(entries) != 0 {
			return fmt.Errorf("drain and recover all jobs before admission migration (%s not empty)", dir)
		}
	}
	return s.writeAtomic("schema", []byte(want))
}
