package scaleset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Concurrent callers start together. Auth refresh is serialized by the SDK's
// original lock; session refresh uses the same expired snapshot for every
// caller. Compare full request multisets (scheduler order is unspecified),
// results and errors.Is, and assert exact refresh/cache request counts.
func TestDiagnosticRefreshConcurrencyMatrix(t *testing.T) {
	for _, mode := range []string{"auth-ok", "auth-registration-fail", "auth-admin-fail", "auth-timeout", "auth-cancel", "session-ok", "session-fail", "session-cancel", "session-timeout", "close-fail", "partial-acquire", "message-200"} {
		t.Run(mode, func(t *testing.T) {
			type result struct {
				Requests, Returns            []string
				Registration, Admin, Refresh int
			}
			run := func(on bool) (result, []byte) {
				var mu sync.Mutex
				var trace []string
				registration, admin, refresh := 0, 0, 0
				var output bytes.Buffer
				var d *Diagnostics
				if on {
					d = diagTest(t, &output, 4096)
				}
				token, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))}).SignedString([]byte("SECRET-jwt"))
				id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					b, _ := io.ReadAll(r.Body)
					trace = append(trace, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+string(b))
					w.Header().Set("Content-Type", "application/json")
					switch {
					case strings.HasSuffix(r.URL.Path, "registration-token"):
						registration++
						if mode == "auth-registration-fail" {
							w.WriteHeader(403)
							io.WriteString(w, `{"message":"SECRET-denied"}`)
							return
						}
						w.WriteHeader(201)
						io.WriteString(w, `{"token":"SECRET-registration"}`)
					case strings.HasSuffix(r.URL.Path, "runner-registration"):
						admin++
						if mode == "auth-admin-fail" {
							w.WriteHeader(403)
							io.WriteString(w, `{"message":"SECRET-denied"}`)
							return
						}
						json.NewEncoder(w).Encode(map[string]string{"url": server.URL, "token": token})
					case r.Method == "PATCH":
						refresh++
						if mode == "session-fail" {
							w.WriteHeader(403)
							io.WriteString(w, `{"message":"SECRET-denied"}`)
							return
						}
						json.NewEncoder(w).Encode(RunnerScaleSetSession{SessionID: id, MessageQueueURL: server.URL + "/queue", MessageQueueAccessToken: "SECRET-new"})
					case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/sessions"):
						json.NewEncoder(w).Encode(RunnerScaleSetSession{SessionID: id, MessageQueueURL: server.URL + "/queue", MessageQueueAccessToken: "SECRET-old"})
					case r.Method == "DELETE":
						if mode == "close-fail" {
							w.WriteHeader(403)
							return
						}
						w.WriteHeader(204)
					case strings.HasSuffix(r.URL.Path, "/acquirejobs"):
						io.WriteString(w, `{"value":[7]}`)
					case r.Method == "GET" && r.URL.Path == "/queue":
						io.WriteString(w, `{"messageId":12,"messageType":"RunnerScaleSetJobMessages","body":"[]"}`)
					default:
						t.Errorf("unexpected route %s", r.URL.Path)
						w.WriteHeader(403)
					}
				}))
				defer server.Close()
				c, e := newClient(testSystemInfo, server.URL+"/org", actionsAuth{token: "SECRET-PAT"}, WithDiagnostics(d), WithRetryWaitMax(time.Nanosecond), WithProxy(func(*http.Request) (*url.URL, error) { return nil, nil }))
				if e != nil {
					t.Fatal(e)
				}
				// Preload the parent token for session-only cases. Auth cases use
				// the uninitialized token, then exercise its cached result too.
				var s *MessageSessionClient
				if !strings.HasPrefix(mode, "auth-") {
					s, e = c.MessageSessionClient(context.Background(), 1, "SECRET-owner")
					if e != nil {
						t.Fatal(e)
					}
				}
				ctx := context.Background()
				if strings.HasSuffix(mode, "cancel") {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if strings.HasSuffix(mode, "timeout") {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				const n = 12
				returns := make([]string, n)
				start := make(chan struct{})
				var wg sync.WaitGroup
				var expired RunnerScaleSetSession
				if s != nil {
					expired = s.Session()
				}
				for i := range n {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						var err error
						var value any
						switch {
						case strings.HasPrefix(mode, "auth-"):
							value, err = c.updateTokenIfNeeded(ctx)
						case mode == "close-fail":
							err = s.Close(ctx)
						case mode == "partial-acquire":
							value, err = s.AcquireJobs(ctx, []int64{7, 8})
						case mode == "message-200":
							value, err = s.GetMessage(ctx, 0, 3)
						default:
							err = s.refreshMessageSession(ctx, expired)
						}
						b, _ := json.Marshal(value)
						text := string(b)
						if err != nil {
							text = err.Error()
						}
						returns[i] = strings.ReplaceAll(text, server.URL, "LOOPBACK") + boolString(errors.Is(err, context.Canceled)) + boolString(errors.Is(err, context.DeadlineExceeded))
					}()
				}
				close(start)
				wg.Wait()
				if mode == "auth-ok" {
					for range n {
						if _, e = c.updateTokenIfNeeded(ctx); e != nil {
							t.Fatal(e)
						}
					}
				}
				if d != nil && !finishDiag(t, d) {
					t.Fatal("diagnostic loss")
				}
				for i := range trace {
					trace[i] = strings.ReplaceAll(trace[i], server.URL, "LOOPBACK")
				}
				sort.Strings(trace)
				sort.Strings(returns)
				if mode == "auth-ok" && (registration != 1 || admin != 1) {
					t.Fatal("refresh/cache duplication", registration, admin)
				}
				if mode == "session-ok" && refresh != 1 {
					t.Fatal("duplicate session refresh", refresh)
				}
				if mode == "session-fail" && refresh != n {
					t.Fatal("missing failed refresh callers", refresh)
				}
				if (strings.HasSuffix(mode, "cancel") || strings.HasSuffix(mode, "timeout")) && refresh != 0 {
					t.Fatal("canceled refresh issued request")
				}
				return result{trace, returns, registration, admin, refresh}, output.Bytes()
			}
			a, _ := run(false)
			b, events := run(true)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("refresh/request/return drift\n%+v\n%+v", a, b)
			}
			if bytes.Contains(events, []byte("SECRET")) || bytes.Contains(events, []byte("01234567-89ab-cdef-0123-456789abcdef")) {
				t.Fatal("secret reached sink")
			}
			if dir := os.Getenv("DIAGNOSTIC_TRACE_DIR"); dir != "" {
				v, _ := json.MarshalIndent(map[string]any{"baseline": a, "diagnostic": b, "events": string(events)}, "", "  ")
				if e := os.WriteFile(filepath.Join(dir, mode+".json"), v, 0600); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func boolString(v bool) string {
	if v {
		return " true"
	}
	return " false"
}
