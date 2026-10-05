package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDatabaseReadinessCannotDelayOtherJobLifetime(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var deletes atomic.Int32
	var publication atomic.Bool
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.47")
		switch {
		case r.Method == "DELETE":
			deletes.Add(1)
			w.WriteHeader(204)
		case p == "/containers/expiring/json":
			json.NewEncoder(w).Encode(obj{"Created": time.Now().Add(-time.Hour), "State": obj{"Running": true}})
		case strings.HasSuffix(p, "/exec"):
			var v struct{ Cmd []string }
			json.NewDecoder(r.Body).Decode(&v)
			id := "request"
			if len(v.Cmd) > 0 && v.Cmd[0] == "pg_isready" {
				id = "readiness"
			}
			if strings.Contains(strings.Join(v.Cmd, " "), "ci-postgres.env") {
				publication.Store(true)
			}
			json.NewEncoder(w).Encode(obj{"Id": id})
		case p == "/exec/readiness/start":
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(500)
		case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
			json.NewEncoder(w).Encode(obj{"Running": false, "ExitCode": 0})
		case strings.HasSuffix(p, "/json"):
			json.NewEncoder(w).Encode(obj{"Config": obj{"Labels": obj{"ci-runner.owner": "unit", "ci-runner.job": "requesting"}, "Env": []string{"POSTGRES_PASSWORD=" + strings.Repeat("a", 48)}}, "State": obj{"Running": true}, "NetworkSettings": obj{"Networks": obj{"requesting-net": obj{"IPAddress": "172.20.0.4"}}}})
		default:
			w.WriteHeader(200)
		}
	})
	f := &fleet{owner: "unit", jobs: map[string]string{"requesting": "requesting-net", "expiring": "expiring-net"}, lifetime: time.Minute}
	f.state("requesting").created = time.Now()
	f.state("expiring").created = time.Now().Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.provisionDatabaseContext(ctx, "requesting", "requesting-net") }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("readiness path not reached")
	}
	f.reap()
	if deletes.Load() != 5 {
		t.Fatal("database readiness blocked unrelated absolute lifetime")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker ignores cancellation")
	}
	if publication.Load() {
		t.Fatal("failed readiness published credentials")
	}
}
