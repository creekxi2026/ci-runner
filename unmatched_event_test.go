package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestUnaddressableEventDoesNotStarveDemand(t *testing.T) {
	f, _, _ := journalFixture(t)
	calls := 0
	f.demandSource = func(context.Context) (int, error) { calls++; return 0, nil }
	event := &scaleset.JobCompleted{RunnerName: "../unaddressable", RunnerID: 0}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 34, JobCompletedMessages: []*scaleset.JobCompleted{event}}
	if err := f.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("demand starved", calls)
	}
	entries, err := os.ReadDir(f.cleanupLog.root.Name())
	if err != nil || len(entries) != 1 {
		t.Fatal("missing durable disposition", entries, err)
	}
}

// Real Scale -> listen with a failed durable commit, failed second event,
// successful retry, and failed ack. No live network or Docker is involved.
func TestUnaddressableEventListenRetries(t *testing.T) {
	f, n, _ := journalFixture(t)
	base := scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "repo", WorkflowRunID: 34, JobID: "opaque"}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 34, JobCompletedMessages: []*scaleset.JobCompleted{
		{RunnerName: "../SECRET", RunnerID: 42, JobMessageBase: base, Result: "canceled"},
		{RunnerName: n, RunnerID: 42, JobMessageBase: base, Result: "succeeded"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &dispositionSession{msg: msg, cancel: cancel}
	f.session = s
	syncDir := f.cleanupLog.syncDir
	sentinel := errors.New("sync sentinel")
	commits := 0
	f.cleanupLog.syncDir = func() error {
		commits++
		if commits == 1 {
			return sentinel
		}
		return syncDir()
	}
	demand := 0
	f.demandSource = func(context.Context) (int, error) { demand++; return 0, nil }
	deletes := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "SECRET") {
			t.Fatal("untrusted path reached Docker")
		}
		if r.Method == "DELETE" {
			deletes++
			if deletes == 1 {
				w.WriteHeader(503)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/containers/json") || strings.HasSuffix(r.URL.Path, "/networks") {
			fmt.Fprint(w, `[]`)
			return
		}
		w.WriteHeader(404)
	})
	// observe commits: first audit fails; on second call audit commits, valid
	// cleanup fails; next retry completes cleanup and reaches demand then ack.
	s.beforeAck = func() {
		if commits < 3 || deletes < 2 || f.state(n) != nil || demand != 2 {
			t.Fatalf("premature ack commits=%d deletes=%d demand=%d", commits, deletes, demand)
		}
		entries, _ := os.ReadDir(f.cleanupLog.root.Name())
		audit := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".audit") {
				audit++
			}
		}
		if audit != 1 {
			t.Fatal("audit missing/duplicated", audit)
		}
	}
	if err := f.listen(ctx, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.acks != 2 {
		t.Fatal("ack retry missing", s.acks)
	}
	if _, ok := f.messages[34]; ok {
		t.Fatal("acked progress retained")
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.Native.Result != "succeeded" || v.ReleasedAt == "" {
		t.Fatal("normal cleanup lost", v, err)
	}
}

type dispositionSession struct {
	msg       *scaleset.RunnerScaleSetMessage
	cancel    context.CancelFunc
	acks      int
	beforeAck func()
}

func (s *dispositionSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{}
}
func (s *dispositionSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	return ids, nil
}
func (s *dispositionSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if s.acks >= 2 {
		s.cancel()
		return nil, ctx.Err()
	}
	return s.msg, nil
}
func (s *dispositionSession) DeleteMessage(context.Context, int) error {
	s.beforeAck()
	s.acks++
	if s.acks == 1 {
		return errors.New("ack fault")
	}
	return nil
}

func TestUnaddressableEventPersistence(t *testing.T) {
	f, _, _ := journalFixture(t)
	event := &scaleset.JobStarted{RunnerName: "../SECRET", RunnerID: 42}
	if _, err := f.recordUnaddressableEvent(event.RunnerName, "started", event); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.cleanupLog.root.Name())
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	name := entries[0].Name()
	path := filepath.Join(f.cleanupLog.root.Name(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("SECRET")) {
		t.Fatal("secret persisted")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0444 {
		t.Fatal(st.Mode())
	}
	// Reopen the journal exactly as startup does; replay must preserve bytes.
	root := f.cleanupLog.root.Name()
	f.cleanupLog.root.Close()
	c, err := openCleanupJournal(f.work, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.root.Close()
	f.cleanupLog = c
	for range 2 {
		if _, err = f.recordUnaddressableEvent(event.RunnerName, "started", event); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, name))
	if !bytes.Equal(data, after) {
		t.Fatal("restart rewrote disposition")
	}
	delete(f.jobs, "ci-job-12345678-abc")
	if err = f.recoverCleanupObservations(); err != nil {
		t.Fatal("audit mistaken for allocation", err)
	}
	// An unsafe existing record must never be overwritten or acknowledged.
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = f.recordUnaddressableEvent(event.RunnerName, "started", event); err == nil {
		t.Fatal("unsafe record accepted")
	}
}

func TestUnaddressableEventSyncCrashAndConcurrency(t *testing.T) {
	f, _, _ := journalFixture(t)
	c := f.cleanupLog
	event := &scaleset.JobCompleted{RunnerName: "../unaddressable"}
	sentinel := errors.New("sync failure")
	syncDir := c.syncDir
	c.syncDir = func() error { return sentinel }
	for range 2 {
		if err := c.recordUnaddressable("completed", event); err != sentinel {
			t.Fatal("lost unsynced rename", err)
		}
	}
	c.syncDir = syncDir
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.recordUnaddressable("completed", event); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	entries, _ := os.ReadDir(c.root.Name())
	if len(entries) != 1 {
		t.Fatal("not idempotent", entries)
	}
	// Different payloads cannot overwrite one another.
	event.Result = "canceled"
	if err := c.recordUnaddressable("completed", event); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(c.root.Name())
	if len(entries) != 2 {
		t.Fatal("distinct event lost")
	}
}

func TestUnaddressableEventRetainsLegitimateJournalWithoutMemory(t *testing.T) {
	f, n, _ := journalFixture(t)
	base := scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "repo", WorkflowRunID: 34, JobID: "opaque"}
	if err := f.observeScheduler(n, 42, base, ""); err != nil {
		t.Fatal(err)
	}
	delete(f.jobs, n)
	// Same ID does not authorize mapping an invalid name onto that resource.
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 34, JobCompletedMessages: []*scaleset.JobCompleted{
		{RunnerName: "../unknown", RunnerID: 42, JobMessageBase: base, Result: "canceled"},
		{RunnerName: n, RunnerID: 42, JobMessageBase: base, Result: "succeeded"},
	}}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("event touched absent/foreign resource: %s", r.URL)
		w.WriteHeader(503)
	})
	if err := f.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	v, err := f.cleanupLog.read(n)
	if err != nil || v.Native.Result != "succeeded" {
		t.Fatal("durable local record ignored", v, err)
	}
	// A contradictory local identity is never waived by the routing rule.
	f.jobs["../unknown"] = "impossible-net"
	if _, err = f.recordUnaddressableEvent("../unknown", "completed", msg.JobCompletedMessages[0]); err == nil {
		t.Fatal("namespace invariant violation ignored")
	}
}

func TestUnaddressableEventUnsafeRecordsAndCrashTemp(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "writable", "content", "owner", "crash-temp"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := journalFixture(t)
			c := f.cleanupLog
			event := &scaleset.JobStarted{RunnerName: "../SECRET"}
			if err := c.recordUnaddressable("started", event); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(c.root.Name())
			path := filepath.Join(c.root.Name(), entries[0].Name())
			foreign := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				os.Remove(path)
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "writable":
				os.Chmod(path, 0644)
			case "content":
				data, _ := os.ReadFile(path)
				os.Remove(path)
				data[0] = '!'
				os.WriteFile(path, data, 0444)
			case "owner":
				c.uid++
			case "crash-temp":
				os.Remove(path)
				if err := os.Symlink(foreign, path+".new"); err != nil {
					t.Fatal(err)
				}
			}
			err := c.recordUnaddressable("started", event)
			if (kind == "crash-temp") != (err == nil) {
				t.Fatal(kind, err)
			}
			data, _ := os.ReadFile(foreign)
			if string(data) != "preserve" {
				t.Fatal("foreign file changed")
			}
		})
	}
}

func TestUnaddressableEventDoesNotReplaceLeaseRecovery(t *testing.T) {
	f, n, _ := journalFixture(t)
	if err := f.work.reserve(n, false); err != nil {
		t.Fatal(err)
	}
	before, err := f.work.root.ReadFile(f.work.leaseDir + "/" + n)
	if err != nil {
		t.Fatal(err)
	}
	delete(f.jobs, n)
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("recovery mutated resources")
		}
		if strings.HasSuffix(r.URL.Path, "/volumes") {
			fmt.Fprint(w, `{"Volumes":[]}`)
		} else {
			fmt.Fprint(w, `[]`)
		}
	})
	event := &scaleset.JobCompleted{RunnerName: "unrelated-name", RunnerID: 42}
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 34, JobCompletedMessages: []*scaleset.JobCompleted{event}}); err != nil {
		t.Fatal(err)
	}
	after, _ := f.work.root.ReadFile(f.work.leaseDir + "/" + n)
	if !bytes.Equal(before, after) {
		t.Fatal("lease lost")
	}
	if err := f.recover(); err != nil {
		t.Fatal(err)
	}
	j := f.state(n)
	if j == nil || !j.recovered || !j.shared || !j.cleaning {
		t.Fatal("durable cleanup responsibility not recovered", j)
	}
}

func TestUnaddressableEventCommitFailureStopsBeforeNextEvent(t *testing.T) {
	f, n, _ := journalFixture(t)
	sentinel := errors.New("durability unavailable")
	f.cleanupLog.syncDir = func() error { return sentinel }
	demand := 0
	f.demandSource = func(context.Context) (int, error) { demand++; return 0, nil }
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 34, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "../unknown"}, {RunnerName: n}}}
	if err := f.Scale(context.Background(), msg); err != sentinel {
		t.Fatal("changed/lost failure", err)
	}
	if demand != 0 || f.messages[34].done || f.state(n) == nil {
		t.Fatal("advanced before durable disposition")
	}
	if _, err := f.cleanupLog.read(n); !os.IsNotExist(err) {
		t.Fatal("second event reached", err)
	}
}
