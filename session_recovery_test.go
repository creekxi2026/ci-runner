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

type freshSession struct {
	fakeSession
	stats *scaleset.RunnerScaleSetStatistic
}

func (s *freshSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{Statistics: s.stats}
}
func (s *freshSession) GetMessage(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.cancel()
	return nil, context.Canceled
}

func TestFreshSessionRecoversAlreadyAcquiredDemand(t *testing.T) {
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
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{networks[name]: obj{"IPAddress": "172.20.0.3"}}}})
		case strings.HasSuffix(p, "/exec"):
			io.WriteString(w, `{"Id":"ready"}`)
		case strings.Contains(p, "/wait"):
			io.WriteString(w, `{"StatusCode":0}`)
		default:
			w.WriteHeader(200)
		}
	})
	api := &fakeRunners{}
	f := &fleet{client: api, set: 42, jobs: map[string]string{}}
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		f.session = &freshSession{fakeSession: fakeSession{cancel: cancel}, stats: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
		_ = f.listen(ctx, time.Millisecond)
		if len(f.snapshot()) != 1 {
			t.Fatal("startup ignored already acquired demand")
		}
		for n, net := range f.snapshot() {
			f.cleanup(n, net)
		}
	}
	if api.generated != 2 {
		t.Fatal("fresh session demand incorrectly deduplicated")
	}
}

type numberedSession struct {
	fakeSession
	next int
}

func (s *numberedSession) GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
	s.next++
	if s.next > 128 {
		s.cancel()
		return nil, context.Canceled
	}
	return &scaleset.RunnerScaleSetMessage{MessageID: s.next}, nil
}
func (s *numberedSession) DeleteMessage(context.Context, int) error { return nil }
func TestAcknowledgedMessagesDoNotGrowCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fleet{session: &numberedSession{fakeSession: fakeSession{cancel: cancel}}, jobs: map[string]string{}}
	_ = f.listen(ctx, time.Millisecond)
	if len(f.messages) != 0 {
		t.Fatalf("retained %d acknowledged messages indefinitely", len(f.messages))
	}
}
