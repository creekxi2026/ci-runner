package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/actions/scaleset"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIdleClaimAmbiguousCompletionRetriedWithoutNewMkdir(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			creates, inspected, deletes := 0, 0, 0
			withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "DELETE":
					deletes++
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					creates++
					json.NewEncoder(w).Encode(obj{"Id": "claim"})
				case strings.HasSuffix(r.URL.Path, "/start"):
					if lost {
						// The daemon accepted the start but its HTTP response was lost.
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					} else {
						w.WriteHeader(200)
					}
				case strings.Contains(r.URL.Path, "/exec/"):
					inspected++
					code := 0
					if creates > 1 {
						code = 1
					} // mkdir loses to the controller's first claim
					json.NewEncoder(w).Encode(obj{"Running": inspected == 1, "ExitCode": code})
				default:
					json.NewEncoder(w).Encode(obj{"Created": time.Now().Add(-10 * time.Minute), "State": obj{"Running": true}})
				}
			})
			f := &fleet{jobs: map[string]string{"idle": "idle-net"}, idle: time.Second, lifetime: time.Hour}
			f.state("idle").gate = true
			f.reap()
			if deletes != 0 {
				t.Fatal("ambiguous running claim killed runner")
			}
			f.reap()
			if deletes != 5 || creates != 1 {
				t.Fatalf("controller's successful claim stranded idle runner: deletes=%d mkdir=%d inspect=%d", deletes, creates, inspected)
			}
		})
	}
}

func TestIdleClaimWithoutExitStatusNeverKills(t *testing.T) {
	deletes := 0
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deletes++
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			json.NewEncoder(w).Encode(obj{"Id": "not-started"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(500)
		case strings.Contains(r.URL.Path, "/exec/"):
			json.NewEncoder(w).Encode(obj{"Running": false, "ExitCode": nil})
		default:
			json.NewEncoder(w).Encode(obj{"Created": time.Now().Add(-time.Minute), "State": obj{"Running": true}})
		}
	})
	f := &fleet{jobs: map[string]string{"idle": "net"}, idle: time.Second, lifetime: time.Hour}
	f.state("idle").gate = true
	f.reap()
	f.reap()
	if deletes != 0 {
		t.Fatal("unstarted/unknown exec mistaken for won atomic gate")
	}
}

func TestSchedulerAssignmentDoesNotPreventOfflineIdleCleanup(t *testing.T) {
	deletes := 0
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deletes++
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			json.NewEncoder(w).Encode(obj{"Id": "claim"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(200)
		case strings.Contains(r.URL.Path, "/exec/"):
			json.NewEncoder(w).Encode(obj{"Running": false, "ExitCode": 0})
		default:
			json.NewEncoder(w).Encode(obj{"Created": time.Now().Add(-10 * time.Minute), "State": obj{"Running": true}})
		}
	})
	f := &fleet{jobs: map[string]string{"idle": "idle-net"}, idle: time.Second, lifetime: time.Hour}
	f.state("idle").gate = true
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "idle"}}}); err != nil {
		t.Fatal(err)
	}
	f.reap()
	if deletes != 5 {
		t.Fatalf("assignment without executing job hook stranded runner: deletes=%d", deletes)
	}
}
