package scaleset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every endpoint is a loopback fixture; unexpected routes fail closed. Both
// modes use the same server, deterministic replies and default SDK policies.
func TestDiagnosticSDKTraceEquivalence(t *testing.T) {
	for _, mode := range []string{"ok", "registration401", "registration403", "registration429", "registration500", "admin-missing", "create-error", "decode", "poll-refresh", "ack-refresh", "acquire-refresh", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var trace []string
			var failed bool
			var server *httptest.Server
			token, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}).SignedString([]byte("SECRET-admin-key"))
			sessionID := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
			handler := func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				b, _ := io.ReadAll(r.Body)
				trace = append(trace, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+string(b))
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "registration-token"):
					for _, v := range []struct {
						name string
						code int
					}{{"registration401", 401}, {"registration403", 403}, {"registration429", 429}, {"registration500", 500}} {
						if mode == v.name {
							w.WriteHeader(v.code)
							io.WriteString(w, `{"message":"SECRET-response"}`)
							return
						}
					}
					w.WriteHeader(201)
					io.WriteString(w, `{"token":"SECRET-registration"}`)
				case strings.HasSuffix(r.URL.Path, "runner-registration"):
					if mode == "admin-missing" {
						io.WriteString(w, `{}`)
						return
					}
					json.NewEncoder(w).Encode(map[string]string{"url": server.URL, "token": token})
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/sessions"):
					if mode == "create-error" {
						w.WriteHeader(403)
						io.WriteString(w, `{"message":"SECRET-response"}`)
						return
					}
					json.NewEncoder(w).Encode(RunnerScaleSetSession{SessionID: sessionID, MessageQueueURL: server.URL + "/queue?secret=SECRET-query", MessageQueueAccessToken: "SECRET-queue"})
				case r.Method == "PATCH":
					json.NewEncoder(w).Encode(RunnerScaleSetSession{SessionID: sessionID, MessageQueueURL: server.URL + "/queue?secret=SECRET-query", MessageQueueAccessToken: "SECRET-refreshed"})
				case r.Method == "GET" && r.URL.Path == "/queue":
					if mode == "poll-refresh" && !failed {
						failed = true
						w.WriteHeader(401)
						return
					}
					if mode == "decode" {
						io.WriteString(w, `SECRET-invalid-json`)
						return
					}
					w.WriteHeader(202)
				case strings.HasSuffix(r.URL.Path, "/acquirejobs"):
					if mode == "acquire-refresh" && !failed {
						failed = true
						w.WriteHeader(401)
						return
					}
					io.WriteString(w, `{"value":[7]}`)
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/queue/"):
					if mode == "ack-refresh" && !failed {
						failed = true
						w.WriteHeader(401)
						return
					}
					w.WriteHeader(204)
				case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/sessions/"):
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}
			server = httptest.NewServer(http.HandlerFunc(handler))
			defer server.Close()
			run := func(on bool) ([]string, string, []byte) {
				mu.Lock()
				trace = nil
				failed = false
				mu.Unlock()
				var out bytes.Buffer
				var d *Diagnostics
				if on {
					d = diagTest(t, &out, 4096)
				}
				c, e := newClient(testSystemInfo, server.URL+"/org", actionsAuth{token: "SECRET-PAT"}, WithDiagnostics(d), WithRetryWaitMax(time.Nanosecond), WithProxy(func(*http.Request) (*url.URL, error) { return nil, nil }))
				if e != nil {
					t.Fatal(e)
				}
				session, e := c.MessageSessionClient(context.Background(), 1, "SECRET-owner")
				var result string
				if e == nil {
					ctx := context.Background()
					if mode == "cancel" {
						v, cancel := context.WithCancel(ctx)
						cancel()
						ctx = v
					}
					if mode == "timeout" {
						v, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
						defer cancel()
						ctx = v
					}
					_, e = session.GetMessage(ctx, 0, 3)
					if e == nil {
						_, e = session.AcquireJobs(ctx, []int64{7})
					}
					if e == nil {
						e = session.DeleteMessage(ctx, 1)
					}
					closeErr := session.Close(context.Background())
					if e == nil {
						e = closeErr
					}
				}
				if e != nil {
					result = e.Error() + fmt.Sprint(errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded))
				}
				if d != nil && !finishDiag(t, d) {
					t.Fatal("diagnostic loss")
				}
				mu.Lock()
				snapshot := append([]string(nil), trace...)
				mu.Unlock()
				return snapshot, result, out.Bytes()
			}
			baseline, baselineError, _ := run(false)
			patched, patchedError, evidence := run(true)
			if !reflect.DeepEqual(baseline, patched) || baselineError != patchedError {
				t.Fatalf("trace/return drift: %v %v / %q %q", baseline, patched, baselineError, patchedError)
			}
			if bytes.Contains(evidence, []byte("SECRET")) {
				t.Fatal("secret reached sink")
			}
			if directory := os.Getenv("DIAGNOSTIC_TRACE_DIR"); directory != "" {
				b, _ := json.MarshalIndent(map[string]any{"baseline": baseline, "patched": patched, "baseline_error": baselineError, "patched_error": patchedError, "events": string(evidence)}, "", "  ")
				if e := os.WriteFile(directory+"/"+mode+".json", b, 0600); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
