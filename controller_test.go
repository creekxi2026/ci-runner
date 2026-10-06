package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/actions/scaleset"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSets struct {
	existing *scaleset.RunnerScaleSet
	creates  int
}

func (f *fakeSets) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	return f.existing, nil
}
func (f *fakeSets) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.creates++
	return s, nil
}
func (f *fakeSets) UpdateRunnerScaleSet(_ context.Context, _ int, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return s, nil
}
func TestRestartReusesExistingScaleSet(t *testing.T) {
	f := &fakeSets{existing: &scaleset.RunnerScaleSet{ID: 42, Name: "same"}}
	s, err := ensureScaleSet(context.Background(), f, "same")
	if err != nil || s.ID != 42 || f.creates != 0 {
		t.Fatal("restart tried duplicate scale-set creation")
	}
}
func TestFirstStartCreatesScaleSetWithUpdatesEnabled(t *testing.T) {
	f := &fakeSets{}
	s, err := ensureScaleSet(context.Background(), f, "new")
	if err != nil || f.creates != 1 || s.RunnerSetting.DisableUpdate {
		t.Fatal("initial scale-set configuration incorrect")
	}
}

func TestCapacity(t *testing.T) {
	for _, v := range []struct{ want, current, n int }{{9, 0, 3}, {2, 1, 1}, {0, 2, 0}, {3, 3, 0}} {
		if n := additions(v.want, v.current, 3); n != v.n {
			t.Fatalf("capacity got %d want %d", n, v.n)
		}
	}
}
func withEngine(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	old := engine
	engine = &http.Client{Transport: handlerTransport{h}}
	t.Cleanup(func() { engine = old })
}

type handlerTransport struct{ h http.HandlerFunc }

func (r handlerTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.h(w, q)
	return w.Result(), nil
}
func TestCleanupFailureRetainsRetryState(t *testing.T) {
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	f := &fleet{jobs: map[string]string{"owned-job": "owned-net"}}
	f.cleanup("owned-job", "owned-net")
	if len(f.jobs) != 1 {
		t.Fatal("cleanup failure forgot resource; cannot retry")
	}
}
func TestCleanupSuccessRemovesState(t *testing.T) {
	var paths []string
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { paths = append(paths, r.URL.Path); w.WriteHeader(204) })
	f := &fleet{jobs: map[string]string{"owned-job": "owned-net"}}
	f.cleanup("owned-job", "owned-net")
	if len(f.jobs) != 0 || len(paths) != 6 {
		t.Fatal("cleanup omitted job companions")
	}
}
func TestDeniedEventsNeverAcquire(t *testing.T) {
	f := &fleet{jobs: map[string]string{}}
	for _, event := range []string{"pull_request", "pull_request_target", "workflow_run", "push"} {
		if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{EventName: event}}}}); err != nil {
			t.Fatal(err)
		}
	}
	// A nil session would panic if any denied job were acquired.
}
func TestJobSecurityContract(t *testing.T) {
	var config map[string]any
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/create") {
			json.NewDecoder(r.Body).Decode(&config)
		}
		w.WriteHeader(201)
	})
	f := &fleet{owner: "unit"}
	if err := f.create("job", "image", "1001", []string{"runner"}, nil, secure(), "internal-job"); err != nil {
		t.Fatal(err)
	}
	h := config["HostConfig"].(map[string]any)
	if config["User"] != "1001" || h["NetworkMode"] != "internal-job" || h["Binds"] != nil || h["Privileged"] == true || h["CapAdd"] != nil {
		t.Fatal("job acquired host authority")
	}
	if h["CapDrop"].([]any)[0] != "ALL" {
		t.Fatal("job capabilities not dropped")
	}
}

func TestInitialStatisticsDoNotReplayDemand(t *testing.T) {
	calls := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	f := &fleet{jobs: map[string]string{}}
	err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: -1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3}})
	if err != nil || calls != 0 {
		t.Fatalf("stale initial statistics triggered provisioning: calls=%d err=%v", calls, err)
	}
}

func TestFailedNetworkCreationRetainsCleanupRetry(t *testing.T) {
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	f := &fleet{jobs: map[string]string{}}
	if err := f.start(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if len(f.jobs) != 1 {
		t.Fatal("ambiguous create and failed cleanup lost retry state")
	}
}

type fakeSession struct {
	acquired   int
	gets, acks int
	msg        *scaleset.RunnerScaleSetMessage
	cancel     context.CancelFunc
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{}
}
func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.acquired++
	return ids, nil
}
func (s *fakeSession) GetMessage(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.gets++
	if s.gets == 1 {
		return nil, fmt.Errorf("poll failure")
	}
	if s.acks >= 2 {
		s.cancel()
		return nil, ctx.Err()
	}
	return s.msg, nil
}
func (s *fakeSession) DeleteMessage(context.Context, int) error {
	s.acks++
	if s.acks == 1 {
		return fmt.Errorf("ack failure")
	}
	return nil
}
func TestRedeliveryAcquiresOnce(t *testing.T) {
	s := &fakeSession{}
	f := &fleet{session: s, jobs: map[string]string{}}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 7, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10, EventName: "workflow_dispatch"}}}}
	for range 2 {
		if err := f.Scale(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if s.acquired != 1 {
		t.Fatalf("redelivery acquired %d times", s.acquired)
	}
}
func TestFailedProvisionKeepsActiveAndDoesNotLockFleet(t *testing.T) {
	f := &fleet{jobs: map[string]string{"busy": "busy-net"}}
	locked := false
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if !f.mu.TryLock() {
			locked = true
		} else {
			f.mu.Unlock()
		}
		if r.Method == "GET" {
			io.WriteString(w, `{"State":{"Running":true}}`)
			return
		}
		if r.Method == "DELETE" && strings.Contains(r.URL.Path, "busy") {
			t.Error("deleted active survivor")
		}
		w.WriteHeader(500)
	})
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}); err == nil {
		t.Fatal("failure falsely acknowledged")
	}
	if _, ok := f.jobs["busy"]; !ok {
		t.Fatal("lost active job")
	}
	if locked {
		t.Fatal("fleet mutex held across Docker I/O")
	}
}
