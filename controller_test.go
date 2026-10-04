package main

import (
	"context"
	"encoding/json"
	"github.com/actions/scaleset"
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
		if n := additions(v.want, v.current); n != v.n {
			t.Fatalf("capacity got %d want %d", n, v.n)
		}
	}
}
func withEngine(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(h)
	old := engine
	engine = &http.Client{Transport: rewriteTransport{server.URL}}
	t.Cleanup(func() { engine = old; server.Close() })
}

type rewriteTransport struct{ base string }

func (r rewriteTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	clone := q.Clone(q.Context())
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return http.DefaultTransport.RoundTrip(clone)
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
	if len(f.jobs) != 0 || len(paths) != 5 {
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
