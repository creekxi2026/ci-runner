package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestPoolLimitConfiguration(t *testing.T) {
	for _, v := range []struct {
		value string
		want  int
	}{{"", 3}, {"1", 1}, {"3", 3}} {
		n, e := poolJobLimit(v.value)
		if e != nil || n != v.want {
			t.Fatal(v, n, e)
		}
	}
	for _, v := range []string{"0", "-1", "4", "bad"} {
		if _, e := poolJobLimit(v); e == nil {
			t.Fatal("accepted", v)
		}
	}
	if additions(9, 1, 1) != 0 || additions(9, 0, 1) != 1 || additions(9, 4, 3) != 0 {
		t.Fatal("configured capacity ignored")
	}
}

func TestConcurrentStartsCountFailedCleanup(t *testing.T) {
	// Failed provisioning and failed cleanup must still consume capacity.
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	for _, limit := range []int{1, 3} {
		f := &fleet{maxJobs: limit, jobs: map[string]string{}}
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = f.start(context.Background()) }()
		}
		wg.Wait()
		if n := len(f.snapshot()); n != limit {
			t.Fatalf("failed/in-flight jobs count %d want %d", n, limit)
		}
	}
}

func TestPoolRestartWithReducedLimitAndNoGlobalLock(t *testing.T) {
	p := t.TempDir()
	s, e := openSharedWork(p, "fixture", "pool", 3)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e = s.reserve(fmt.Sprintf("ci-job-%08x-000", i), false); e != nil {
			t.Fatal(e)
		}
	}
	s.Close()
	s, e = openSharedWork(p, "fixture", "pool", 1)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// A different process holding the old global lock cannot block runtime work.
	lock, e := os.OpenFile(filepath.Join(p, "admission.lock"), os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e = s.reserve("ci-job-ffffffff-000", false); !errors.Is(e, errCapacity) {
			t.Fatalf("over-limit recovery admitted job: %v", e)
		}
		if e = s.release(fmt.Sprintf("ci-job-%08x-000", i)); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.reserve("ci-job-ffffffff-000", false); e != nil {
		t.Fatal(e)
	}
}

func TestAdmissionMigrationRequiresDrainedStoppedPools(t *testing.T) {
	p := t.TempDir()
	s, e := openSharedWork(p, "fixture", "pool-a", 1)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	old := sharedWorkSchema + "\nfixture\nlimit=3\n"
	if e = os.WriteFile(filepath.Join(p, "schema"), []byte(old), 0600); e != nil {
		t.Fatal(e)
	}
	// An old controller with no jobs still must be stopped before format change.
	f, e := os.OpenFile(filepath.Join(p, "owners", "pool-b"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	if x, e := openSharedWork(p, "fixture", "pool-a", 1); e == nil {
		x.Close()
		t.Fatal("migrated while another controller alive")
	}
	f.Close()
	orphan := filepath.Join(p, "leases", "ci-job-00000000-000")
	if e = os.WriteFile(orphan, []byte("retained evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	if x, e := openSharedWork(p, "fixture", "pool-a", 1); e == nil {
		x.Close()
		t.Fatal("discarded old lease")
	}
	if b, e := os.ReadFile(orphan); e != nil || string(b) != "retained evidence" {
		t.Fatal("old lease changed")
	}
	if e = os.Remove(orphan); e != nil {
		t.Fatal(e)
	}
	s, e = openSharedWork(p, "fixture", "pool-a", 1)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if b, e := os.ReadFile(filepath.Join(p, "schema")); e != nil || string(b) == old {
		t.Fatal("migration not persisted", e)
	}
}

func TestAbruptOwnerExitRetainsReservation(t *testing.T) {
	p := t.TempDir()
	c := exec.Command(os.Args[0], "-test.run=^TestAdmissionProcess$")
	c.Env = append(os.Environ(), "CI_ADMISSION_TEST_ROOT="+p, "CI_ADMISSION_TEST_OWNER=crashed", "CI_ADMISSION_TEST_LIMIT=1", "CI_ADMISSION_TEST_CRASH=1")
	out, e := c.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("child %v %s", e, out)
	}
	s, e := openSharedWork(p, "fixture", "crashed", 1)
	if e != nil {
		t.Fatal("owner lock not released", e)
	}
	defer s.Close()
	all, e := s.owned()
	if e != nil || len(all) != 1 {
		t.Fatal("crashed reservation lost", all, e)
	}
	if e = s.reserve("ci-job-ffffffff-000", false); !errors.Is(e, errCapacity) {
		t.Fatal("crashed reservation not counted", e)
	}
}
