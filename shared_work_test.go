package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestAdmissionProcess(t *testing.T) {
	p := os.Getenv("CI_ADMISSION_TEST_ROOT")
	if p == "" {
		return
	}
	limit, err := poolJobLimit(os.Getenv("CI_ADMISSION_TEST_LIMIT"))
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getenv("CI_ADMISSION_TEST_OWNER")
	s, err := openSharedWork(p, "fixture", owner, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.reserve(fmt.Sprintf("ci-job-%08x-000", i), false)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if !errors.Is(err, errCapacity) {
				t.Errorf("reserve: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if os.Getenv("CI_ADMISSION_TEST_CRASH") == "1" {
		os.Exit(86)
	}
	if accepted != limit {
		t.Fatalf("accepted %d, want %d", accepted, limit)
	}
}
func TestIndependentAdmissionAcrossProcesses(t *testing.T) {
	p := t.TempDir()
	init, err := openSharedWork(p, "fixture", "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	init.Close()
	var wg sync.WaitGroup
	for i, limit := range []int{1, 3} {
		wg.Add(1)
		go func(i, limit int) {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestAdmissionProcess$")
			c.Env = append(os.Environ(), "CI_ADMISSION_TEST_ROOT="+p, fmt.Sprintf("CI_ADMISSION_TEST_OWNER=pool-%d", i), fmt.Sprintf("CI_ADMISSION_TEST_LIMIT=%d", limit))
			if out, e := c.CombinedOutput(); e != nil {
				t.Errorf("child: %v %s", e, out)
			}
		}(i, limit)
	}
	wg.Wait()
	for i, limit := range []int{1, 3} {
		s, e := openSharedWork(p, "fixture", fmt.Sprintf("pool-%d", i), limit)
		if e != nil {
			t.Fatal(e)
		}
		all, e := s.owned()
		if e != nil || len(all) != limit {
			t.Fatalf("restart lost records: %v %v", all, e)
		}
		if e = s.reserve("ci-job-ffffffff-001", false); !errors.Is(e, errCapacity) {
			t.Fatal("restart admitted excess", e)
		}
		for n := range all {
			if e = s.release(n); e != nil {
				t.Fatal(e)
			}
			break
		}
		if e = s.reserve("ci-job-ffffffff-002", false); e != nil {
			t.Fatal("freed local slot unavailable", e)
		}
		s.Close()
	}
}
func TestSharedWorkIsolationAndRestart(t *testing.T) {
	p := t.TempDir()
	s, e := openSharedWork(p, "fixture", "pool-a")
	if e != nil {
		t.Fatal(e)
	}
	if other, e := openSharedWork(p, "fixture", "pool-a"); e == nil {
		other.Close()
		t.Fatal("duplicate controller owner admitted")
	}
	n := "ci-job-00000001-000"
	if e = s.reserve(n, false); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.root.Mkdir("jobs/"+n, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, filepath.Join(p, "jobs", n, "escape")); e != nil {
		t.Fatal(e)
	}
	if e = s.release(n); e == nil {
		t.Fatal("released before cleanup")
	}
	s.Close()
	s, e = openSharedWork(p, "fixture", "pool-a")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	owned, e := s.owned()
	if e != nil || len(owned) != 1 {
		t.Fatal(owned, e)
	}
	if e = s.removeData(n); e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(outside, "keep")); e != nil || string(b) != "keep" {
		t.Fatal("cleanup escaped job directory")
	}
	if e = s.release(n); e != nil {
		t.Fatal(e)
	}
	if e = s.reserve("../escape", false); e == nil {
		t.Fatal("invalid job path accepted")
	}
	if wrong, e := openSharedWork(p, "wrong-volume", "pool-b"); e == nil {
		wrong.Close()
		t.Fatal("volume identity mismatch allowed")
	}
}

func TestOtherPoolCannotConsumeCapacity(t *testing.T) {
	p := t.TempDir()
	a, e := openSharedWork(p, "fixture", "pool-a", 1)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := openSharedWork(p, "fixture", "pool-b", 3)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if e = a.reserve("ci-job-00000000-000", false); e != nil {
		t.Fatal(e)
	}
	// Even unreadable/corrupt records in a stopped foreign pool do not affect b.
	if e = os.WriteFile(filepath.Join(p, "pool-leases", "pool-a", "ci-job-00000000-000"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e = b.reserve(fmt.Sprintf("ci-job-%08x-001", i), false); e != nil {
			t.Fatal(e)
		}
	}
	f := &fleet{maxJobs: 3, work: b, changes: make(chan struct{}, 1), demandSource: func(context.Context) (int, error) { return 1, nil }}
	if e = f.reconcileCurrent(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(f.snapshot()) != 0 {
		t.Fatal("created phantom job")
	}
}
