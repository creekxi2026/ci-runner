package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// No Docker socket or GitHub endpoint is used. The real provisioning and
// cleanup methods run against a stateful finite Docker transport.
type resourceJIT struct {
	fakeRunners
	mode  string
	calls *[]string
}

func (a *resourceJIT) GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	*a.calls = append(*a.calls, "JIT "+s.Name)
	if a.mode == "jit" {
		return nil, errors.New("SECRET-jit-error")
	}
	return a.fakeRunners.GenerateJitRunnerConfig(ctx, s, set)
}

var resourceJobName = regexp.MustCompile(`ci-job-[a-f0-9-]{12}`)

func TestStartResourceTraceMatrix(t *testing.T) {
	for _, mode := range []string{"success", "reserve", "capacity", "network", "proxy-create", "proxy-start", "proxy-connect", "proxy-address", "jit", "disk", "worker-create", "worker-start", "firewall", "release", "cleanup-failure", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			type result struct {
				Trace                        []string
				Error                        string
				Canceled, Deadline, Capacity bool
				Jobs, States                 int
			}
			run := func() result {
				var trace []string
				api := &resourceJIT{mode: mode, calls: &trace}
				f := &fleet{client: api, set: 42, owner: "fixture", image: "fixture-image", netout: "fixture-egress", jobs: map[string]string{}}
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
				engine.Transport = resourceTraceTransport{underlying, &trace}
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
				text := ""
				if err != nil {
					text = resourceJobName.ReplaceAllString(err.Error(), "ci-job-FIXTURE")
				}
				for i := range trace {
					trace[i] = resourceJobName.ReplaceAllString(trace[i], "ci-job-FIXTURE")
				}
				r := result{trace, text, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, errCapacity), len(f.jobs), len(f.states)}
				if mode == "cancel" && (!r.Canceled || r.Jobs != 0) {
					t.Fatalf("cancel semantics changed: %+v", r)
				}
				if mode == "timeout" && !r.Deadline {
					t.Fatalf("timeout semantics changed: %+v", r)
				}
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
				return r
			}
			run()
		})
	}
}

type resourceTraceTransport struct {
	base  http.RoundTripper
	trace *[]string
}

func (x resourceTraceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	*x.trace = append(*x.trace, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	return x.base.RoundTrip(r)
}
