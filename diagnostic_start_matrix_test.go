package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// No Docker socket or GitHub endpoint is used. The real provisioning and
// cleanup methods run against a stateful finite Docker transport.
type diagnosticJIT struct {
	fakeRunners
	mode  string
	calls *[]string
}

func (a *diagnosticJIT) GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	*a.calls = append(*a.calls, "JIT "+s.Name)
	if a.mode == "jit" {
		return nil, errors.New("SECRET-jit-error")
	}
	return a.fakeRunners.GenerateJitRunnerConfig(ctx, s, set)
}

type diagnosticFaultWriter struct {
	mode    string
	release chan struct{}
}

func (w diagnosticFaultWriter) Write(b []byte) (int, error) {
	switch w.mode {
	case "panic":
		panic("SECRET-writer")
	case "blocked":
		<-w.release
		return len(b), nil
	case "short":
		return 0, nil
	default:
		return 0, errors.New("SECRET-EIO-ENOSPC")
	}
}

var diagnosticJobName = regexp.MustCompile(`ci-job-[a-f0-9-]{12}`)

func TestDiagnosticStartResourceTraceMatrix(t *testing.T) {
	for _, mode := range []string{"success", "reserve", "capacity", "network", "proxy-create", "proxy-start", "proxy-connect", "proxy-address", "jit", "disk", "worker-create", "worker-start", "firewall", "release", "cleanup-failure", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			type result struct {
				Trace                        []string
				Error                        string
				Canceled, Deadline, Capacity bool
				Jobs, States                 int
			}
			run := func(writer string) (result, []byte) {
				var output bytes.Buffer
				var d *scaleset.Diagnostics
				release := make(chan struct{})
				if writer != "nil" {
					var sink io.Writer = &output
					if writer != "healthy" {
						sink = diagnosticFaultWriter{writer, release}
					}
					var err error
					d, err = scaleset.NewDiagnostics(scaleset.DiagnosticIdentity{WindowID: "start-matrix", SourceRevision: strings.Repeat("a", 40), ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "fixture"}, [32]byte{1}, sink, 1)
					if err != nil {
						t.Fatal(err)
					}
				}
				var trace []string
				api := &diagnosticJIT{mode: mode, calls: &trace}
				f := &fleet{diagnostics: d, client: api, set: 42, owner: "fixture", image: "fixture-image", netout: "fixture-egress", jobs: map[string]string{}}
				if mode == "capacity" {
					f.stopping = true
				}
				if mode == "reserve" {
					work, e := openSharedWork(t.TempDir(), "fixture", "fixture-pool", 1)
					if e != nil {
						t.Fatal(e)
					}
					defer work.Close()
					if e = work.reserve("ci-job-00000000-000", false); e != nil {
						t.Fatal(e)
					}
					f.work = work
				}
				networks := map[string]string{}
				waits := 0
				withJobDiskEngine(t, func(w http.ResponseWriter, r *http.Request) {
					p := r.URL.Path
					if strings.Contains(p, "-fw/wait") {
						waits++
					}
					name := r.URL.Query().Get("name")
					fail := mode == "network" && strings.HasSuffix(p, "/networks/create") || mode == "proxy-create" && strings.HasSuffix(name, "-proxy") || mode == "proxy-start" && strings.HasSuffix(p, "-proxy/start") || mode == "proxy-connect" && strings.HasSuffix(p, "/connect") || mode == "proxy-address" && strings.HasSuffix(p, "-proxy/json") || mode == "disk" && strings.Contains(p, "-fw/wait") && waits == 1 || mode == "worker-create" && name != "" && name == jobName(name) || mode == "worker-start" && strings.HasSuffix(p, "/start") && strings.Contains(p, "/containers/") && !strings.Contains(p, "-proxy/") && !strings.Contains(p, "-fw/") || mode == "firewall" && strings.Contains(p, "-fw/wait") && waits == 2 || mode == "release" && strings.HasSuffix(p, "/exec/ready/start") || mode == "cleanup-failure" && (r.Method == "DELETE" || strings.HasSuffix(p, "/networks/create"))
					if fail {
						w.WriteHeader(403)
						io.WriteString(w, "SECRET-body")
						return
					}
					switch {
					case strings.HasSuffix(p, "/containers/create"):
						var v struct{ HostConfig map[string]any }
						json.NewDecoder(r.Body).Decode(&v)
						networks[name], _ = v.HostConfig["NetworkMode"].(string)
						w.WriteHeader(201)
					case strings.HasSuffix(p, "/json"):
						n := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
						json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{networks[n]: obj{"IPAddress": "172.20.0.3"}}}})
					case strings.Contains(p, "/wait"):
						io.WriteString(w, `{"StatusCode":0}`)
					case strings.HasSuffix(p, "/exec"):
						io.WriteString(w, `{"Id":"ready"}`)
					default:
						w.WriteHeader(204)
					}
				})
				underlying := engine.Transport
				engine.Transport = diagnosticTraceTransport{underlying, &trace}
				ctx := context.Background()
				if mode == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if mode == "timeout" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				err := f.start(ctx)
				close(release)
				if d != nil {
					closeDiagnostics(d)
				}
				text := ""
				if err != nil {
					text = diagnosticJobName.ReplaceAllString(err.Error(), "ci-job-FIXTURE")
				}
				for i := range trace {
					trace[i] = diagnosticJobName.ReplaceAllString(trace[i], "ci-job-FIXTURE")
				}
				r := result{trace, text, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, errCapacity), len(f.jobs), len(f.states)}
				if mode == "success" {
					if err != nil || r.Jobs != 1 || api.generated != 1 {
						t.Fatalf("success fixture failed: %+v", r)
					}
				} else if err == nil {
					t.Fatalf("%s fault not reached", mode)
				}
				if mode != "success" && mode != "release" && mode != "cleanup-failure" && r.Jobs != 0 {
					t.Fatalf("unexpected retained resources: %+v", r)
				}
				return r, output.Bytes()
			}
			baseline, _ := run("nil")
			for _, writer := range []string{"healthy", "short", "io", "panic", "blocked"} {
				got, events := run(writer)
				if !reflect.DeepEqual(baseline, got) {
					t.Fatalf("%s changed resource/error trace\n%+v\n%+v", writer, baseline, got)
				}
				if bytes.Contains(events, []byte("SECRET")) {
					t.Fatal("secret reached diagnostics")
				}
				if out := os.Getenv("DIAGNOSTIC_TRACE_DIR"); out != "" {
					b, _ := json.MarshalIndent(map[string]any{"baseline": baseline, "diagnostic": got, "writer": writer}, "", "  ")
					if e := os.WriteFile(filepath.Join(out, fmt.Sprintf("start-%s-%s.json", mode, writer)), b, 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
		})
	}
}

type diagnosticTraceTransport struct {
	base  http.RoundTripper
	trace *[]string
}

func (x diagnosticTraceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	*x.trace = append(*x.trace, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	return x.base.RoundTrip(r)
}
