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
	owner := os.Getenv("CI_ADMISSION_TEST_OWNER")
	s, e := openSharedWork(p, "fixture", owner)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	e = s.reserve(os.Getenv("CI_ADMISSION_TEST_JOB"), false)
	if errors.Is(e, errCapacity) {
		os.Exit(42)
	}
	if e != nil {
		t.Fatal(e)
	}
}
func TestGlobalAdmissionAcrossProcesses(t *testing.T) {
	p := t.TempDir()
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestAdmissionProcess$")
			c.Env = append(os.Environ(), "CI_ADMISSION_TEST_ROOT="+p, fmt.Sprintf("CI_ADMISSION_TEST_OWNER=pool-%d", i), fmt.Sprintf("CI_ADMISSION_TEST_JOB=ci-job-%08x-000", i))
			out, e := c.CombinedOutput()
			if e == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
				return
			}
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 42 {
				t.Errorf("child: %v %s", e, out)
			}
		}(i)
	}
	wg.Wait()
	if accepted != 3 {
		t.Fatalf("accepted %d, want exactly 3 globally", accepted)
	}
	s, e := openSharedWork(p, "fixture", "audit")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	all, e := s.leases()
	if e != nil || len(all) != 3 {
		t.Fatalf("leases %v %v", all, e)
	}
	// Crashed/restarted owners retain their slots; only the owning deployment can
	// release one, then another pool can use it without an event from its own job.
	for n, l := range all {
		owner, e := openSharedWork(p, "fixture", l.Owner)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.release(n); e == nil {
			t.Fatal("foreign release permitted")
		}
		if e = owner.release(n); e != nil {
			t.Fatal(e)
		}
		owner.Close()
		break
	}
	if e = s.reserve("ci-job-ffffffff-000", false); e != nil {
		t.Fatal(e)
	}
	if e = s.reserve("ci-job-ffffffff-001", false); !errors.Is(e, errCapacity) {
		t.Fatal(e)
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

func TestGlobalCapacityReschedulesWithoutBlockingEvents(t *testing.T) {
	p := t.TempDir()
	a, e := openSharedWork(p, "fixture", "pool-a")
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := openSharedWork(p, "fixture", "pool-b")
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	for i := 0; i < 3; i++ {
		if e := a.reserve(fmt.Sprintf("ci-job-%08x-000", i), false); e != nil {
			t.Fatal(e)
		}
	}
	f := &fleet{work: b, changes: make(chan struct{}, 1), demandSource: func(context.Context) (int, error) { return 1, nil }}
	if e := f.reconcileCurrent(context.Background()); e != nil {
		t.Fatal("full global budget must not block listener completion", e)
	}
	if len(f.snapshot()) != 0 {
		t.Fatal("capacity rejection created phantom job")
	}
	select {
	case <-f.changes:
	default:
		t.Fatal("waiting pool won't retry after another pool releases")
	}
}
