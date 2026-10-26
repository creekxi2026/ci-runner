package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/actions/scaleset"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func withProvisioningEngine(t *testing.T) {
	networks := map[string]string{}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			var body struct{ HostConfig map[string]any }
			json.NewDecoder(r.Body).Decode(&body)
			networks[r.URL.Query().Get("name")] = body.HostConfig["NetworkMode"].(string)
			w.WriteHeader(201)
		case strings.HasSuffix(p, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
			json.NewEncoder(w).Encode(obj{"State": obj{"Running": true}, "NetworkSettings": obj{"Networks": obj{networks[name]: obj{"IPAddress": "172.20.0.3"}}}})
		case strings.HasSuffix(p, "/exec"):
			io.WriteString(w, `{"Id":"ready"}`)
		case strings.Contains(p, "/wait"):
			io.WriteString(w, `{"StatusCode":0}`)
		default:
			w.WriteHeader(200)
		}
	})
}

type pendingSession struct {
	freshSession
	reads   int
	pending *scaleset.RunnerScaleSetMessage
}

func (s *pendingSession) GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
	s.reads++
	if s.reads > 1 {
		s.cancel()
		return nil, context.Canceled
	}
	return s.pending, nil
}
func (s *pendingSession) DeleteMessage(context.Context, int) error { return nil }

func TestNewSessionDoesNotDuplicatePendingCreationPlan(t *testing.T) {
	withProvisioningEngine(t)
	api := &fakeRunners{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fleet{client: api, set: 42, jobs: map[string]string{"busy": "busy-net"}, messages: map[int]*messageProgress{7: {acquired: true}}, demandSource: func(context.Context) (int, error) { return 2, nil }}
	f.session = &pendingSession{freshSession: freshSession{fakeSession: fakeSession{cancel: cancel}, stats: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}, pending: &scaleset.RunnerScaleSetMessage{MessageID: 7, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}}
	_ = f.listen(ctx, time.Millisecond)
	if api.generated != 1 || len(f.snapshot()) != 2 {
		t.Fatalf("duplicated pending plan: JIT=%d jobs=%d", api.generated, len(f.snapshot()))
	}
}

func TestRetiredSnapshotCannotResurrectState(t *testing.T) {
	f := &fleet{jobs: map[string]string{"retired": "net"}}
	_ = f.state("retired")
	f.mu.Lock()
	delete(f.jobs, "retired")
	delete(f.states, "retired")
	f.mu.Unlock()
	if f.state("retired") != nil || len(f.states) != 0 {
		t.Fatal("stale snapshot resurrected retired state")
	}
}

func TestLocalCleanupRechecksUnservedDemand(t *testing.T) {
	withProvisioningEngine(t)
	api := &fakeRunners{}
	f := &fleet{client: api, set: 42, jobs: map[string]string{"tombstone": "dead-net"}, changes: make(chan struct{}, 1), demandSource: func(context.Context) (int, error) { return 1, nil }}
	f.state("tombstone").cleaning = true
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.coordinate(ctx, time.Millisecond) }()
	f.maintain(ctx, time.Millisecond)
	<-done
	if api.generated != 1 || len(f.snapshot()) != 1 {
		t.Fatalf("cleanup lost assigned demand: JIT=%d jobs=%d", api.generated, len(f.snapshot()))
	}
}

// Retained from the former diagnostics-equivalence test: a transient bootstrap
// demand failure must be retried before the session proceeds.
func TestBootstrapDemandFailureRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &fakeSession{cancel: cancel, msg: &scaleset.RunnerScaleSetMessage{MessageID: 8}}
	calls := 0
	fleet := &fleet{session: session, jobs: map[string]string{}, demandSource: func(context.Context) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("fixture demand unavailable")
		}
		return 0, nil
	}}
	err := fleet.listen(ctx, time.Millisecond)
	if calls < 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap demand retry: calls=%d error=%v", calls, err)
	}
}
