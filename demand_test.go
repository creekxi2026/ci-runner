package main

import (
	"context"
	"encoding/json"
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
	f.maintain(ctx, time.Millisecond)
	if api.generated != 1 || len(f.snapshot()) != 1 {
		t.Fatalf("cleanup lost assigned demand: JIT=%d jobs=%d", api.generated, len(f.snapshot()))
	}
}
