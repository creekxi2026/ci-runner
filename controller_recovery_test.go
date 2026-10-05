package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/actions/scaleset"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPollingAndAckRetryPreserveActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeSession{cancel: cancel, msg: &scaleset.RunnerScaleSetMessage{MessageID: 8, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9, EventName: "workflow_dispatch"}}}}}
	f := &fleet{session: s, jobs: map[string]string{"busy": "busy-net"}}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("polling touched Docker: %s", r.URL.Path)
		w.WriteHeader(500)
	})
	_ = f.listen(ctx, time.Millisecond)
	if s.gets < 3 || s.acks != 2 || s.acquired != 1 || len(f.jobs) != 1 {
		t.Fatalf("retry state: gets=%d acks=%d acquired=%d jobs=%d", s.gets, s.acks, s.acquired, len(f.jobs))
	}
}
func TestRecoveryAdoptsLiveCleansOrphanAndHonorsLifetime(t *testing.T) {
	live := "ci-job-12345678-123"
	orphan := "ci-job-87654321-321"
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	deleted := map[string]bool{}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.47")
		switch {
		case r.Method == "DELETE":
			deleted[p] = true
			w.WriteHeader(204)
		case p == "/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), "ci-runner.owner=unit") {
				t.Error("unscoped recovery")
			}
			json.NewEncoder(w).Encode([]obj{{"Names": []string{"/" + live}, "Labels": obj{"ci-runner.owner": "unit"}}, {"Names": []string{"/" + live + "-pg"}, "Labels": obj{"ci-runner.owner": "unit"}}, {"Names": []string{"/" + orphan + "-pg"}, "Labels": obj{"ci-runner.owner": "unit"}}, {"Names": []string{"/foreign"}, "Labels": obj{"ci-runner.owner": "other"}}})
		case p == "/networks":
			json.NewEncoder(w).Encode([]obj{{"Name": live + "-net", "Labels": obj{"ci-runner.owner": "unit"}, "Created": old}, {"Name": orphan + "-net", "Labels": obj{"ci-runner.owner": "unit"}, "Created": old}})
		case p == "/containers/"+live+"/json":
			fmt.Fprintf(w, `{"Created":%q,"State":{"Running":true}}`, old)
		default:
			w.WriteHeader(404)
		}
	})
	f := &fleet{owner: "unit", jobs: map[string]string{}, lifetime: time.Hour}
	if err := f.recover(); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.jobs[live]; !ok {
		t.Fatal("live job not adopted")
	}
	if deleted["/containers/"+live] || deleted["/containers/"+live+"-pg"] {
		t.Fatal("recovery killed live work")
	}
	f.reap()
	if !deleted["/containers/"+live] || !deleted["/containers/"+orphan+"-pg"] {
		t.Fatal("restart reset lifetime or failed orphan cleanup")
	}
	if deleted["/containers/foreign"] {
		t.Fatal("foreign resource deleted")
	}
}
func TestIdleCancellationAndBusyGate(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			deletes := 0
			withEngine(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "DELETE":
					deletes++
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					io.WriteString(w, `{"Id":"gate"}`)
				case strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(200)
				case strings.Contains(r.URL.Path, "/exec/"):
					code := 0
					if busy {
						code = 1
					}
					fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
				default:
					fmt.Fprintf(w, `{"Created":%q,"State":{"Running":true}}`, time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339Nano))
				}
			})
			f := &fleet{jobs: map[string]string{"idle": "idle-net"}, idle: 300 * time.Second, lifetime: time.Hour}
			f.state("idle").gate = true
			f.reap()
			if !busy && deletes != 5 {
				t.Fatalf("canceled preassignment runner not reclaimed: %d", deletes)
			}
			if busy && deletes != 0 {
				t.Fatal("busy gate ignored")
			}
		})
	}
}
func TestTimeoutBounds(t *testing.T) {
	for _, value := range []string{"0", "-1", "invalid", "999999999999999999", "86401"} {
		t.Setenv("RUNNER_MAX_LIFETIME_SECONDS", value)
		if _, _, err := timeoutConfig(); err == nil {
			t.Fatalf("accepted invalid lifetime %q", value)
		}
	}
}

type fakeRunners struct {
	generated, removed int
	lookup             *scaleset.RunnerReference
	removeErr          error
}

func (c *fakeRunners) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	c.generated++
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "test-only", Runner: &scaleset.RunnerReference{ID: 1, Name: s.Name, RunnerScaleSetID: set}}, nil
}
func (c *fakeRunners) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return c.lookup, nil
}
func (c *fakeRunners) RemoveRunner(context.Context, int64) error { c.removed++; return c.removeErr }
func TestCompletionCleanupFailureIsNotAcknowledged(t *testing.T) {
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	f := &fleet{jobs: map[string]string{"job": "net"}}
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 3, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "job"}}}); err == nil {
		t.Fatal("failed completion cleanup falsely acknowledged")
	}
}
func TestPartialCleanupRetriesAndRetainsSurvivor(t *testing.T) {
	fail := true
	deleted := map[string]int{}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted[r.URL.Path]++
			if fail && strings.Contains(r.URL.Path, "-pg") {
				w.WriteHeader(500)
			} else {
				w.WriteHeader(204)
			}
			return
		}
		io.WriteString(w, `{"State":{"Running":true}}`)
	})
	f := &fleet{jobs: map[string]string{"dead": "dead-net", "busy": "busy-net"}}
	f.cleanup("dead", "dead-net")
	if len(f.jobs) != 2 {
		t.Fatal("lost partial cleanup")
	}
	fail = false
	f.reap()
	if len(f.jobs) != 1 || f.jobs["busy"] == "" || deleted["/v1.47/containers/busy"] != 0 || deleted["/v1.47/containers/dead-pg"] != 2 {
		t.Fatal("partial cleanup did not resume independently")
	}
}
func TestProvisioningPayloadAndRedeliveryHardMax(t *testing.T) {
	api := &fakeRunners{}
	networks := map[string]string{}
	created := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			var v struct {
				User       string
				Labels     map[string]string
				HostConfig map[string]any
			}
			json.NewDecoder(r.Body).Decode(&v)
			name := r.URL.Query().Get("name")
			networks[name] = v.HostConfig["NetworkMode"].(string)
			created++
			if name == jobName(name) {
				if v.HostConfig["ReadonlyRootfs"] != true {
					t.Error("runner root filesystem is unbounded writable storage")
				}
				tmpfs, _ := v.HostConfig["Tmpfs"].(map[string]any)
				if !strings.Contains(fmt.Sprint(tmpfs["/home/runner"]), "size=2g") || fmt.Sprint(tmpfs["/tmp"]) != "rw,exec,size=1g,nr_inodes=32768,mode=1777" || !strings.Contains(fmt.Sprint(tmpfs["/home/runner"]), "rw,exec,") {
					t.Error("runner needs bounded 1 GiB executable temp storage with original inode/permission limits")
				}
				if v.HostConfig["Memory"].(float64) != 4*1024*1024*1024 {
					t.Error("runner memory must remain bounded at the 4 GiB budget")
				}
			}
			if v.Labels["ci-runner.owner"] != "unit" || v.Labels["ci-runner.job"] != jobName(name) {
				t.Error("missing recovery identity")
			}
			if !strings.HasSuffix(name, "-fw") && (v.HostConfig["CapAdd"] != nil || v.HostConfig["Privileged"] == true || v.HostConfig["Binds"] != nil) {
				t.Error("extra job authority")
			}
			if v.HostConfig["Memory"].(float64) <= 0 || v.HostConfig["NanoCpus"].(float64) <= 0 || v.HostConfig["PidsLimit"].(float64) <= 0 {
				t.Error("unbounded job resource")
			}
			w.WriteHeader(201)
		case strings.HasSuffix(p, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{networks[name]: obj{"IPAddress": "172.20.0.3"}}}})
		case strings.Contains(p, "/wait"):
			io.WriteString(w, `{"StatusCode":0}`)
		case strings.HasSuffix(p, "/exec"):
			io.WriteString(w, `{"Id":"ready"}`)
		default:
			w.WriteHeader(200)
		}
	})
	f := &fleet{client: api, set: 42, owner: "unit", jobs: map[string]string{}}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 10, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 9}}
	for range 2 {
		if err := f.Scale(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if api.generated != 3 || len(f.jobs) != 3 || created != 9 {
		t.Fatalf("overprovision: JIT=%d jobs=%d creates=%d", api.generated, len(f.jobs), created)
	}
	if err := f.start(context.Background()); err == nil {
		t.Fatal("start bypassed hard max")
	}
	f.mu.Lock()
	f.stopping = true
	f.mu.Unlock()
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 11, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 9}}); err == nil {
		t.Fatal("shutdown accepted scheduling")
	}
}
func TestDeregistrationRetryKeepsNetworkTombstone(t *testing.T) {
	api := &fakeRunners{lookup: &scaleset.RunnerReference{ID: 7, Name: "job", RunnerScaleSetID: 42}, removeErr: fmt.Errorf("outage")}
	nets := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/networks/") {
			nets++
		}
		w.WriteHeader(204)
	})
	f := &fleet{client: api, set: 42, unregister: true, jobs: map[string]string{"job": "job-net"}}
	f.cleanup("job", "job-net")
	if nets != 0 || len(f.jobs) != 1 {
		t.Fatal("lost deregistration tombstone")
	}
	api.removeErr = nil
	f.reap()
	if nets != 1 || len(f.jobs) != 0 || api.removed != 2 {
		t.Fatal("did not retry deregistration")
	}
}

func TestCanceledStartDoesNotProvision(t *testing.T) {
	calls := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fleet{jobs: map[string]string{}}
	_ = f.start(ctx)
	if calls != 0 {
		t.Fatal("canceled scheduling created resources")
	}
}
func TestOtherJobReapedWhileProvisioningReservationHeld(t *testing.T) {
	// A published reservation must already have a locked job state: the janitor
	// may run between reserving capacity and the first Docker request.
	f := &fleet{jobs: map[string]string{"starting": "start-net", "dead": "dead-net"}}
	j := f.state("starting")
	j.mu.Lock()
	defer j.mu.Unlock()
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "starting") {
			t.Error("janitor touched provisioning job")
		}
		if r.Method == "GET" {
			io.WriteString(w, `{"State":{"Running":false}}`)
		} else {
			w.WriteHeader(204)
		}
	})
	f.reap()
	if len(f.jobs) != 1 || f.jobs["starting"] == "" {
		t.Fatal("janitor blocked or deleted provisioning job")
	}
}

type lostSession struct{ fakeSession }

func (s *lostSession) GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
	return nil, scaleset.NotFoundError
}
func TestLostSessionReturnsForReacquisitionWithoutCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	f := &fleet{session: &lostSession{}, jobs: map[string]string{"busy": "net"}}
	err := f.listen(ctx, time.Millisecond)
	if !errors.Is(err, scaleset.NotFoundError) || len(f.jobs) != 1 {
		t.Fatalf("lost session not returned intact: %v", err)
	}
}
func TestAmbiguousReadyFailureDoesNotKillPotentiallyBusyRunner(t *testing.T) {
	api := &fakeRunners{}
	networks := map[string]string{}
	deletes := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			var v struct{ HostConfig map[string]any }
			json.NewDecoder(r.Body).Decode(&v)
			networks[r.URL.Query().Get("name")] = v.HostConfig["NetworkMode"].(string)
			w.WriteHeader(201)
		case strings.HasSuffix(p, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{networks[name]: obj{"IPAddress": "172.20.0.3"}}}})
		case strings.Contains(p, "/wait"):
			io.WriteString(w, `{"StatusCode":0}`)
		case strings.HasSuffix(p, "/exec"):
			io.WriteString(w, `{"Id":"ready"}`)
		case p == "/v1.47/exec/ready/start":
			w.WriteHeader(500)
		case r.Method == "DELETE":
			if !strings.HasSuffix(p, "-fw") {
				deletes++
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(200)
		}
	})
	f := &fleet{client: api, set: 42, owner: "unit", jobs: map[string]string{}}
	if f.start(context.Background()) == nil {
		t.Fatal("ambiguous release falsely successful")
	}
	if len(f.jobs) != 1 || deletes != 0 {
		t.Fatal("possibly assigned runner killed after ambiguous release")
	}
}

func TestDrainWaitsForActiveJobAndStopsScheduling(t *testing.T) {
	f := &fleet{jobs: map[string]string{"busy": "net"}, lifetime: time.Second}
	done := make(chan bool, 1)
	go func() { done <- f.drain(time.Millisecond) }()
	select {
	case <-done:
		t.Fatal("reported instantaneous graceful completion with active work")
	case <-time.After(10 * time.Millisecond):
	}
	f.mu.Lock()
	stopped := f.stopping
	delete(f.jobs, "busy")
	f.mu.Unlock()
	if !stopped {
		t.Fatal("drain did not stop scheduling")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("clean drain failed")
		}
	case <-time.After(time.Second):
		t.Fatal("drain failed to finish")
	}
}
func TestDrainIsBoundedDuringCleanupOutage(t *testing.T) {
	f := &fleet{jobs: map[string]string{"busy": "net"}, lifetime: 5 * time.Millisecond}
	if f.drain(time.Millisecond) {
		t.Fatal("claimed complete despite retained resources")
	}
	if len(f.jobs) != 1 {
		t.Fatal("drain forgot recovery resources")
	}
}
func TestJobHookProtectsIdleButNotAbsoluteLifetime(t *testing.T) {
	deletes := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes++
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exec") {
			io.WriteString(w, `{"Id":"hook-won"}`)
		} else if strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json") {
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		} else {
			io.WriteString(w, `{"State":{"Running":true}}`)
		}
	})
	f := &fleet{jobs: map[string]string{"busy": "net"}, idle: time.Second, lifetime: time.Hour}
	j := f.state("busy")
	j.created = time.Now().Add(-time.Minute)
	j.gate = true
	if err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 12, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "busy"}}, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}}); err != nil {
		t.Fatal(err)
	}
	f.reap()
	if deletes != 0 {
		t.Fatal("stale zero demand killed busy job")
	}
	j.created = time.Now().Add(-2 * time.Hour)
	f.reap()
	if deletes != 5 {
		t.Fatal("job state bypassed hard lifetime")
	}
}
func TestLocalJanitorOperatesWithoutSession(t *testing.T) {
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"State":{"Running":false}}`)
		} else {
			w.WriteHeader(204)
		}
	})
	f := &fleet{jobs: map[string]string{"dead": "net"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.maintain(ctx, time.Millisecond); close(done) }()
	deadline := time.Now().Add(time.Second)
	for len(f.snapshot()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if len(f.snapshot()) != 0 {
		t.Fatal("local cleanup required GitHub session")
	}
}

func TestPartialMessageRetryDoesNotReplaceAlreadyCompletedCapacity(t *testing.T) {
	api := &fakeRunners{}
	networks := map[string]string{}
	creates := 0
	fail := true
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v1.47/networks/create":
			creates++
			if creates == 2 && fail {
				w.WriteHeader(500)
			} else {
				w.WriteHeader(201)
			}
		case strings.HasSuffix(p, "/containers/create"):
			var v struct{ HostConfig map[string]any }
			json.NewDecoder(r.Body).Decode(&v)
			networks[r.URL.Query().Get("name")] = v.HostConfig["NetworkMode"].(string)
			w.WriteHeader(201)
		case strings.HasSuffix(p, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{networks[name]: obj{"IPAddress": "172.20.0.3"}}}})
		case strings.Contains(p, "/wait"):
			io.WriteString(w, `{"StatusCode":0}`)
		case strings.HasSuffix(p, "/exec"):
			io.WriteString(w, `{"Id":"ready"}`)
		default:
			w.WriteHeader(200)
		}
	})
	want := 2
	f := &fleet{client: api, set: 42, jobs: map[string]string{}, demandSource: func(context.Context) (int, error) { return want, nil }}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 99, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}
	if f.Scale(context.Background(), msg) == nil {
		t.Fatal("expected partial provisioning failure")
	}
	for n, network := range f.snapshot() {
		f.cleanup(n, network)
	} // First runner finished before retry.
	want = 1 // The authenticated service snapshot now excludes completed work.
	fail = false
	if err := f.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if api.generated != 2 {
		t.Fatalf("replayed completed demand: generated=%d", api.generated)
	}
}
func TestDeregisterAlreadyAbsentRunnerCompletesCleanup(t *testing.T) {
	api := &fakeRunners{lookup: &scaleset.RunnerReference{ID: 7, Name: "job", RunnerScaleSetID: 42}, removeErr: scaleset.RunnerNotFoundError}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f := &fleet{client: api, set: 42, unregister: true, jobs: map[string]string{"job": "net"}}
	f.cleanup("job", "net")
	if len(f.jobs) != 0 {
		t.Fatal("already absent registration leaked tombstone")
	}
}
