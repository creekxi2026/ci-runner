package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlockedDemandCannotDelayOtherJobLifetime(t *testing.T) {
	var removed atomic.Bool
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			removed.Store(true)
			w.WriteHeader(204)
			return
		}
		json.NewEncoder(w).Encode(obj{"State": obj{"Running": true}})
	})
	entered := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	f := &fleet{jobs: map[string]string{"expiring": "expiring-net"}, lifetime: 100 * time.Millisecond, changes: make(chan struct{}, 1), demandSource: func(ctx context.Context) (int, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	j := f.state("expiring")
	j.created = time.Now()
	j.busy = true
	f.changed()
	janitorDone, coordinatorDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(janitorDone); f.maintain(ctx, time.Millisecond) }()
	go func() { defer close(coordinatorDone); f.coordinate(ctx, time.Millisecond) }()
	defer func() { cancel(); <-janitorDone; <-coordinatorDone }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("demand path not exercised")
	}
	time.Sleep(250 * time.Millisecond)
	if !removed.Load() {
		t.Fatal("blocked replacement delayed another job's absolute lifetime")
	}
}
