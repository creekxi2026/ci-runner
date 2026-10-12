package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

func journalFixture(t *testing.T) (*fleet, string, string) {
	t.Helper()
	dir := t.TempDir()
	w, e := openSharedWork(dir, "fixture", "unit")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(w.Close)
	c, e := openCleanupJournal(w, uint32(os.Getuid()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.root.Close() })
	n := "ci-job-12345678-abc"
	f := &fleet{owner: "unit", work: w, cleanupLog: c, jobs: map[string]string{n: n + "-net"}}
	return f, n, filepath.Join(dir, "cleanup-v1", "unit", n+".json")
}

func TestCleanupJournalIdentityAndDelayedOutcome(t *testing.T) {
	f, n, _ := journalFixture(t)
	e := scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "repo", WorkflowRunID: 123, JobID: "scheduler-uuid"}
	if err := f.observeScheduler(n, 42, e, ""); err != nil {
		t.Fatal(err)
	}
	delete(f.jobs, n) // Completion can arrive after the janitor deleted the runner.
	if err := f.observeScheduler(n, 42, e, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := f.observeScheduler(n, 42, e, ""); err != nil {
		t.Fatal(err)
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.Native.Result != "failed" {
		t.Fatal(v, err)
	}
	if err = f.observeScheduler(n, 42, e, "succeeded"); err == nil {
		t.Fatal("overwrote original failure")
	}
	e.WorkflowRunID++
	if err = f.observeScheduler(n, 42, e, "failed"); err == nil {
		t.Fatal("accepted changed run")
	}
	if err = f.observeScheduler("../foreign", 42, e, ""); err == nil {
		t.Fatal("accepted path escape")
	}
}

func TestCleanupJournalRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "writable", "oversized", "foreign", "directory", "duplicate", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			f, n, p := journalFixture(t)
			if err := f.cleanupLog.update(n, func(v *cleanupObservation) error { v.StartedAt = "2026-10-13T00:00:00Z"; return nil }); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				os.Rename(p, p+".target")
				err = os.Symlink(p+".target", p)
			case "hardlink":
				err = os.Link(p, p+".link")
			case "writable":
				err = os.Chmod(p, 0644)
			case "oversized":
				os.Remove(p)
				err = os.WriteFile(p, make([]byte, 65537), 0444)
			case "foreign":
				os.Remove(p)
				err = os.WriteFile(p, []byte(strings.Replace(string(b), `"owner_id":"unit"`, `"owner_id":"other"`, 1)), 0444)
			case "directory":
				os.Remove(p)
				err = os.Mkdir(p, 0444)
			case "duplicate":
				os.Remove(p)
				err = os.WriteFile(p, append([]byte(`{"schema":"bad",`), b[1:]...), 0444)
			case "foreign-owner":
				f.cleanupLog.uid++
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.cleanupLog.read(n); err == nil {
				t.Fatal("accepted unsafe file")
			}
			if err = f.cleanupLog.update(n, func(*cleanupObservation) error { return nil }); err == nil {
				t.Fatal("overwrote unsafe evidence")
			}
		})
	}
}

func TestCleanupJournalConcurrencyAndCrashFile(t *testing.T) {
	f, n, p := journalFixture(t)
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(foreign, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, strings.TrimSuffix(p, ".json")+".new"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.cleanupLog.update(n, func(v *cleanupObservation) error {
				if v.StartedAt == "" {
					v.StartedAt = "2026-10-13T00:00:00Z"
				}
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(foreign)
	if err != nil || string(b) != "unchanged" {
		t.Fatal("followed crash symlink")
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.StartedAt == "" {
		t.Fatal(v, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0444 {
		t.Fatal("wrong publication mode")
	}
}

func TestCleanupJournalPartialAndRestart(t *testing.T) {
	for _, remaining := range []string{"none", "container", "network", "lease", "unknown"} {
		t.Run(remaining, func(t *testing.T) {
			f, n, _ := journalFixture(t)
			if err := f.cleanupLog.update(n, func(v *cleanupObservation) error { v.StartedAt = "2026-10-13T00:00:00Z"; return nil }); err != nil {
				t.Fatal(err)
			}
			delete(f.jobs, n)
			if remaining == "lease" {
				if err := f.work.reserve(n, false); err != nil {
					t.Fatal(err)
				}
			}
			withEngine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Fatal("recovery attempted mutation")
				}
				if !strings.Contains(r.URL.Query().Get("filters"), "ci-runner.job="+n) {
					t.Fatal("unscoped resource proof")
				}
				if remaining == "unknown" {
					w.WriteHeader(503)
					return
				}
				if remaining == "container" && strings.Contains(r.URL.Path, "containers") || remaining == "network" && strings.Contains(r.URL.Path, "networks") {
					fmt.Fprint(w, `[{"Id":"remaining"}]`)
				} else {
					fmt.Fprint(w, `[]`)
				}
			})
			err := f.recoverCleanupObservations()
			if (remaining == "none") != (err == nil) {
				t.Fatal(remaining, err)
			}
			v, e := f.cleanupLog.read(n)
			if e != nil {
				t.Fatal(e)
			}
			if (v.ReleasedAt != "") != (remaining == "none") {
				t.Fatal("partial cleanup claimed release")
			}
			if remaining == "none" {
				before, _ := json.Marshal(v)
				if e = f.recoverCleanupObservations(); e != nil {
					t.Fatal(e)
				}
				v, _ = f.cleanupLog.read(n)
				after, _ := json.Marshal(v)
				if string(before) != string(after) {
					t.Fatal("recovery not idempotent")
				}
			}
		})
	}
}

func TestCleanupJournalFailedWritePreservesResources(t *testing.T) {
	f, n, _ := journalFixture(t)
	f.cleanupLog.root.Close()
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("deleted before durable evidence: %s", r.Method)
		w.WriteHeader(500)
	})
	f.cleanup(n, n+"-net")
	if _, ok := f.snapshot()[n]; !ok {
		t.Fatal("forgot job after journal failure")
	}
}

func TestCleanupJournalRetriesAfterReleasedLease(t *testing.T) {
	f, n, _ := journalFixture(t)
	if err := f.work.reserve(n, false); err != nil {
		t.Fatal(err)
	}
	j := f.state(n)
	j.disk = true
	j.shared = true
	fail := true
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/json") || strings.HasSuffix(r.URL.Path, "/networks") {
			if fail {
				w.WriteHeader(503)
			} else {
				fmt.Fprint(w, `[]`)
			}
			return
		}
		w.WriteHeader(404)
	})
	f.cleanup(n, n+"-net")
	if !j.cleanupFinalizing || f.state(n) == nil {
		t.Fatal("did not preserve finalization retry")
	}
	fail = false
	f.cleanup(n, n+"-net")
	if f.state(n) != nil {
		t.Fatal("finalization retry stuck after lease release")
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.ReleasedAt == "" {
		t.Fatal(v, err)
	}
}

func TestCleanupJournalRetriesDirectorySyncAfterRename(t *testing.T) {
	f, n, _ := journalFixture(t)
	c := f.cleanupLog
	syncDir := c.syncDir
	fail := true
	calls := 0
	c.syncDir = func() error {
		calls++
		if fail {
			return fmt.Errorf("injected sync failure")
		}
		return syncDir()
	}
	change := func(v *cleanupObservation) error { v.StartedAt = "2026-10-13T00:00:00Z"; return nil }
	if err := c.update(n, change); err == nil {
		t.Fatal("lost directory sync failure")
	}
	if _, err := c.read(n); err != nil {
		t.Fatal("rename did not commit before injected failure")
	}
	if err := c.update(n, change); err == nil {
		t.Fatal("identical retry incorrectly acknowledged unsynced data")
	}
	fail = false
	if err := c.update(n, change); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("did not retry directory sync")
	}
}

func TestCleanupJournalRetryAfterLeaseUnlinkBeforeSync(t *testing.T) {
	f, n, _ := journalFixture(t)
	if err := f.work.reserve(n, false); err != nil {
		t.Fatal(err)
	}
	if err := f.cleanupLog.update(n, func(v *cleanupObservation) error { v.StartedAt = "2026-10-13T00:00:00Z"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Reproduce release's post-unlink/pre-sync state at the actual boundary.
	if err := f.work.root.Remove(f.work.leaseDir + "/" + n); err != nil {
		t.Fatal(err)
	}
	j := f.state(n)
	j.disk = true
	j.shared = true
	j.cleanupFinalizing = true
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("retried destructive cleanup after lease unlink")
		}
		fmt.Fprint(w, `[]`)
	})
	// Rename the lease directory so its sync fails; do not forget the job.
	if err := f.work.root.Rename(f.work.leaseDir, f.work.leaseDir+"-held"); err != nil {
		t.Fatal(err)
	}
	f.cleanup(n, n+"-net")
	if f.state(n) == nil {
		t.Fatal("ignored release durability failure")
	}
	if err := f.work.root.Rename(f.work.leaseDir+"-held", f.work.leaseDir); err != nil {
		t.Fatal(err)
	}
	f.cleanup(n, n+"-net")
	if f.state(n) != nil {
		t.Fatal("lease unlink retry stuck")
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.ReleasedAt == "" {
		t.Fatal(v, err)
	}
}
