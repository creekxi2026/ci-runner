package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// Test-only translation isolates all ordinary volume-subpaths below a private
// directory in the existing fixed volume. It does not mock Docker operations.
type terminalTestTransport struct {
	base   http.RoundTripper
	prefix string
}

type terminalInspectFault struct {
	base  http.RoundTripper
	path  string
	fired atomic.Bool
	skip  int32
	seen  atomic.Int32
}

func (r *terminalInspectFault) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.Method == "GET" && q.URL.Path == r.path && r.seen.Add(1) > r.skip && r.fired.CompareAndSwap(false, true) {
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"temporary inspect failure"}`)), Request: q}, nil
	}
	return r.base.RoundTrip(q)
}

func (r terminalTestTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/containers/create") && q.Body != nil {
		b, e := io.ReadAll(q.Body)
		if e != nil {
			return nil, e
		}
		q.Body.Close()
		var v map[string]any
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		if host, ok := v["HostConfig"].(map[string]any); ok {
			if mounts, ok := host["Mounts"].([]any); ok {
				for _, item := range mounts {
					m := item.(map[string]any)
					if m["Type"] == "volume" && m["Source"] == "ci-work-linux-arm64" {
						options := m["VolumeOptions"].(map[string]any)
						options["Subpath"] = r.prefix + "/" + options["Subpath"].(string)
					}
				}
			}
		}
		b, e = json.Marshal(v)
		if e != nil {
			return nil, e
		}
		q = q.Clone(q.Context())
		q.Body = io.NopCloser(bytes.NewReader(b))
		q.ContentLength = int64(len(b))
	}
	return r.base.RoundTrip(q)
}

func TestServiceTerminalRuntime(t *testing.T) {
	if os.Getenv("CI_TERMINAL_TEST") != "1" {
		t.Skip("explicit external fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	original := engine
	engine = &http.Client{Transport: terminalTestTransport{original.Transport, os.Getenv("CI_TERMINAL_SUBPATH")}, Timeout: original.Timeout}
	defer func() { engine = original }()
	w, e := openSharedWork("/work", "ci-work-linux-arm64", "ruif106")
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	journal, e := openCleanupJournal(w, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer journal.root.Close()
	b, e := os.ReadFile("/consumer/catalog.json")
	if e != nil {
		t.Fatal(e)
	}
	var catalog serviceCatalog
	if e = strictServiceDecode(b, &catalog); e != nil {
		t.Fatal(e)
	}
	if len(catalog.Services) != 1 {
		t.Fatal("one fixture service required")
	}
	id := ""
	for k := range catalog.Services {
		id = k
	}
	image := os.Getenv("CI_TERMINAL_RUNNER")
	f := &fleet{owner: w.owner, work: w, cleanupLog: journal, image: image, services: &serviceManager{catalog, serviceDigest(catalog)}, jobs: map[string]string{}, lifetime: 4 * time.Minute}
	var native struct {
		Repository string `json:"repository"`
		RunID      int64  `json:"run_id"`
	}
	b, e = os.ReadFile("/consumer/native.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &native); e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(native.Repository, "/")
	if len(parts) != 2 {
		t.Fatal("fixture repository")
	}
	hook := func(t *testing.T, phase, n, outcome string) {
		t.Helper()
		c := exec.CommandContext(ctx, "python3", "/consumer/terminal_fixture.py", phase, n, outcome)
		if out, e := c.CombinedOutput(); e != nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "terminal fixture failed:") {
					t.Log(line)
				}
			}
			t.Fatalf("external fixture %s rejected (diagnostics suppressed)", phase)
		}
	}
	for _, outcome := range []string{"succeeded", "failed", "canceled", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			n := "ci-job-" + uuid.NewString()[:12]
			if e = w.reserve(n, false); e != nil {
				t.Fatal(e)
			}
			f.jobs[n] = n + "-net"
			j := f.state(n)
			j.created = time.Now()
			j.disk = true
			j.shared = true
			defer f.cleanup(n, n+"-net")
			if e = docker("POST", "/networks/create", obj{"Name": n + "-net", "Internal": true, "Labels": f.resourceLabels(n)}, nil); e != nil {
				t.Fatal(e)
			}
			h := secure()
			h["ReadonlyRootfs"] = true
			if e = f.prepareJobDisk(ctx, n, h); e != nil {
				t.Fatal(e)
			}
			index, e := f.prepareServices(n, h)
			if e != nil {
				t.Fatal(e)
			}
			exitCode := 0
			if outcome == "failed" || outcome == "unavailable" {
				exitCode = 7
			}
			workerCommand := []string{"/bin/sh", "-c", fmt.Sprintf("while [ ! -e /tmp/terminal-exit ]; do sleep .1; done; exit %d", exitCode)}
			if e = f.createContext(ctx, n, image, "1001", workerCommand, nil, h, n+"-net"); e != nil {
				t.Fatal(e)
			}
			event := scaleset.JobMessageBase{OwnerName: parts[0], RepositoryName: parts[1], WorkflowRunID: native.RunID, JobID: outcome}
			if e = f.observeScheduler(n, 106, event, ""); e != nil {
				t.Fatal(e)
			}
			if code, e := execCode(ctx, n, "1001", []string{"touch", "/tmp/ci-service-request-" + id}); e != nil || code != 0 {
				t.Fatal("request")
			}
			f.provisionService(ctx, n, id, j.created)
			if _, e = f.readServiceRecord(n, id); !os.IsNotExist(e) {
				t.Fatal("unadmitted fixture started")
			}
			hook(t, "admit", n, outcome)
			f.provisionService(ctx, n, id, j.created)
			r, e := f.readServiceRecord(n, id)
			if e != nil || r.Phase != "ready" {
				t.Fatal("fixture service not ready")
			}
			// Start an external observer in the already-private namespace. The fixture
			// decides how to hold active sessions; the core knows no service semantics.
			if e = f.serviceMkdir(serviceDir(n, id)+"/hold", 0700); e != nil {
				t.Fatal(e)
			}
			host := serviceHost(catalog.Services[id], 2)
			host["Mounts"] = []obj{f.serviceVolumeMount(serviceDir(n, id)+"/outputs", "/outputs", true), f.serviceVolumeMount(serviceDir(n, id)+"/hold", "/hold", false), obj{"Type": "bind", "Source": os.Getenv("CI_TERMINAL_FIXTURE_HOST"), "Target": "/consumer", "ReadOnly": true}}
			if e = f.serviceCreate(ctx, index, id, "check", catalog.Services[id].Adapter, "0", []string{"python3"}, []string{"/consumer/terminal_fixture.py", "hold", n, outcome}, []string{"PYTHONPATH=/opt/closet-ci:/opt/adapter-python"}, host, "container:"+serviceName(n, id, "ns")); e != nil {
				t.Fatal(e)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, e = w.root.Lstat(serviceDir(n, id) + "/hold/ready"); e == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture session holder unavailable")
				}
				time.Sleep(50 * time.Millisecond)
			}

			if outcome == "canceled" {
				exitCode = 137
				if e = docker("POST", "/containers/"+n+"/kill", nil, nil); e != nil {
					t.Fatal(e)
				}
			} else {
				if code, e := execCode(ctx, n, "1001", []string{"touch", "/tmp/terminal-exit"}); e != nil || code != 0 {
					t.Fatal("worker exit trigger")
				}
			}
			var exited struct{ StatusCode int }
			if e = docker("POST", "/containers/"+n+"/wait?condition=not-running", nil, &exited); e != nil || exited.StatusCode != exitCode {
				t.Fatal("worker outcome mismatch")
			}

			if outcome == "failed" {
				// Project fixture interrupts after one destructive operation;
				// the reviewed adapter must recover from its private checkpoint.
				if e = docker("DELETE", "/containers/"+n+"?force=true", nil, nil); e != nil {
					t.Fatal(e)
				}
				interrupted := serviceHost(catalog.Services[id], 2)
				interrupted["Mounts"] = []obj{f.serviceVolumeMount(serviceDir(n, id)+"/init", "/run/ci-adapter", true), f.serviceVolumeMount(serviceDir(n, id)+"/secrets-init", "/run/ci-service-secrets", true), f.serviceVolumeMount(serviceDir(n, id)+"/terminal", "/terminal", false), f.serviceVolumeMount(serviceDir(n, id)+"/hold", "/hold", false), obj{"Type": "bind", "Source": os.Getenv("CI_TERMINAL_FIXTURE_HOST"), "Target": "/consumer", "ReadOnly": true}}
				if e = f.serviceCreate(ctx, index, id, "teardown", catalog.Services[id].Adapter, "0", []string{"python3"}, []string{"/consumer/terminal_fixture.py", "interrupt", n, outcome}, []string{"PYTHONPATH=/opt/closet-ci:/opt/adapter-python"}, interrupted, "container:"+serviceName(n, id, "ns")); e != nil {
					t.Fatal(e)
				}
				if e = f.serviceWait(ctx, serviceName(n, id, "teardown")); e == nil {
					t.Fatal("external partial-cleanup fault did not fail")
				}
				if _, e = w.root.Lstat(serviceDir(n, id) + "/hold/interrupted"); e != nil {
					t.Fatal("fault checkpoint missing")
				}
			}

			if outcome == "succeeded" {
				if e = f.beginCleanupObservation(n); e != nil {
					t.Fatal(e)
				}
				if e = docker("DELETE", "/containers/"+n+"?force=true", nil, nil); e != nil {
					t.Fatal(e)
				}
				// Block the atomic destination before commit, then simulate a
				// restart beyond the deadline with the completed adapter intact.
				tmp := n + ".new"
				if e = journal.root.Mkdir(tmp, 0700); e != nil {
					t.Fatal(e)
				}
				block, e := journal.root.Create(tmp + "/block")
				if e != nil {
					t.Fatal(e)
				}
				block.Close()
				if e = f.collectServiceTeardown(index, id); e == nil {
					t.Fatal("injected journal failure not reached")
				}
				if e = journal.root.RemoveAll(tmp); e != nil {
					t.Fatal(e)
				}
				if e = journal.update(n, func(v *cleanupObservation) error {
					v.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				recovered, e := openCleanupJournal(w, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer func() { f.cleanupLog = journal; recovered.root.Close() }()
				f.cleanupLog = recovered
			}

			// Independent native outcome is simulated; actual worker destruction and
			// service cleanup are real Docker operations for each terminal branch.
			nativeOutcome := outcome
			if outcome == "unavailable" {
				nativeOutcome = "failed"
				if e = docker("POST", "/containers/"+serviceName(n, id, "server")+"/kill", nil, nil); e != nil {
					t.Fatal(e)
				}
			}
			if e = f.observeScheduler(n, 106, event, nativeOutcome); e != nil {
				t.Fatal(e)
			}
			if outcome == "canceled" {
				// A transient server observation must leave the real service and
				// private inputs intact, then recover with the original time bound.
				client := engine
				fault := &terminalInspectFault{base: client.Transport, path: "/v1.47/containers/" + serviceName(n, id, "server") + "/json"}
				engine = &http.Client{Transport: fault, Timeout: client.Timeout}
				f.cleanup(n, n+"-net")
				engine = client
				if !fault.fired.Load() || f.state(n) == nil {
					t.Fatal("transient server observation did not retain pending cleanup")
				}
				if e = f.verifyServiceContainer(ctx, index, id, "server", catalog.Services[id].Image, true); e != nil {
					t.Fatal("proof source was lost after transient inspect", e)
				}
				if _, e = w.root.Lstat(serviceDir(n, id) + "/secrets-init"); e != nil {
					t.Fatal("private recovery inputs were lost", e)
				}
				before, readError := f.cleanupLog.read(n)
				if readError != nil || len(before.Proofs) != 0 {
					t.Fatal("unexpected proof before recovery", readError)
				}
				recovered, openError := openCleanupJournal(w, 0)
				if openError != nil {
					t.Fatal(openError)
				}
				defer func() { f.cleanupLog = journal; recovered.root.Close() }()
				f.cleanupLog = recovered
				after, readError := recovered.read(n)
				if readError != nil || after.StartedAt != before.StartedAt {
					t.Fatal("restart extended the teardown deadline", readError)
				}
				t.Log("server inspect 503 retained real service and private inputs; restarted with original deadline")
				// An error while preparing the adapter has the same retention rule.
				// First inspect proves no completed witness; the second is setup.
				fault = &terminalInspectFault{base: client.Transport, path: "/v1.47/containers/" + serviceName(n, id, "teardown") + "/json", skip: 1}
				engine = &http.Client{Transport: fault, Timeout: client.Timeout}
				f.cleanup(n, n+"-net")
				engine = client
				if !fault.fired.Load() || f.state(n) == nil {
					t.Fatal("adapter setup uncertainty discarded recovery inputs")
				}
				if e = f.verifyServiceContainer(ctx, index, id, "server", catalog.Services[id].Image, true); e != nil {
					t.Fatal("adapter setup error lost proof source", e)
				}
				t.Log("teardown setup inspect 503 retained the service for the next bounded retry")
			}
			f.cleanup(n, n+"-net")
			if f.state(n) != nil {
				t.Fatal("cleanup pending")
			}
			v, e := journal.read(n)
			expectedProofs := 1
			if outcome == "unavailable" {
				expectedProofs = 0
			}
			if e != nil || v.ReleasedAt == "" || len(v.Proofs) != expectedProofs {
				t.Fatal("terminal proof missing")
			}
			if _, e = w.root.Lstat("jobs/" + n); !os.IsNotExist(e) {
				t.Fatal("job disk remains")
			}
			hook(t, "collect", n, outcome)
			t.Log(fmt.Sprintf("outcome=%s worker-exit=%d real-resource-cleanup=true scheduler=fixture private-disk=absent", outcome, exitCode))
		})
	}
}
