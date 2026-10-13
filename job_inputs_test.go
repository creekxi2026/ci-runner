package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/actions/scaleset"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inputFixture(t *testing.T, body string) (*fleet, *jobInputs) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root adapter installation")
	}
	base := t.TempDir()
	command := filepath.Join(base, "adapter.sh")
	script := []byte("#!/bin/sh\nset -eu\n" + body + "\n")
	if e := os.WriteFile(command, script, 0555); e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(script)
	work, e := openSharedWork(filepath.Join(base, "work"), "ci-work-linux-arm64", "unit")
	if os.IsNotExist(e) {
		if e = os.Mkdir(filepath.Join(base, "work"), 0700); e != nil {
			t.Fatal(e)
		}
		work, e = openSharedWork(filepath.Join(base, "work"), "ci-work-linux-arm64", "unit")
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { work.Close() })
	f := &fleet{owner: "unit", work: work, client: &fakeRunners{}, jobs: map[string]string{}, image: "dummy@sha256:" + strings.Repeat("a", 64)}
	f.cleanupLog, e = openCleanupJournal(work, 0)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.cleanupLog.root.Close() })
	policy := inputPolicy{Schema: "ci-job-inputs/v1", Command: []string{command}, Digest: hex.EncodeToString(digest[:]), Mounts: []inputMount{{Name: "payload", Target: "/run/dummy-input"}}}
	data, _ := json.Marshal(policy)
	path := filepath.Join(base, "policy.json")
	if e = os.WriteFile(path, data, 0444); e != nil {
		t.Fatal(e)
	}
	if e = f.openJobInputs(path); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.inputs.root.Close() })
	return f, f.inputs
}
func inputSpec() obj {
	return obj{"Image": "dummy", "Env": []string{"JIT=secret", "proxy=private"}, "HostConfig": obj{"Memory": int64(99), "Mounts": []obj{{"Type": "volume", "Source": "ci-work-linux-arm64", "Target": "/work", "VolumeOptions": obj{"Subpath": "jobs/private"}}}}}
}
func TestInputSealingAssignmentAndRestartFairness(t *testing.T) {
	f, b := inputFixture(t, "cat >/dev/null; exit 75")
	// For preparation use an actual successful sealed adapter, then preserve the
	// waiting exit for reconcile by testing through a shell discriminator.
	body := []byte("#!/bin/sh\nread call\ncase \"$call\" in *prepare*) exit 0;; *) exit 75;; esac\n")
	os.Chmod(b.policy.Command[0], 0755)
	os.WriteFile(b.policy.Command[0], body, 0555)
	h := sha256.Sum256(body)
	b.policy.Digest = hex.EncodeToString(h[:])
	for _, n := range []string{"ci-job-a", "ci-job-b", "ci-job-c"} {
		spec := inputSpec()
		if e := f.prepareJobInputs(context.Background(), n, spec); e != nil {
			t.Fatal(e)
		}
		if spec["HostConfig"].(obj)["Memory"] != int64(99) || len(spec["Env"].([]string)) != 3 {
			t.Fatal("original resources lost")
		}
		event := &scaleset.JobStarted{RunnerName: n, RunnerID: 42, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "dummy", WorkflowRunID: 7, JobID: "opaque", EventName: "push"}}
		if e := f.assignJobInputs(event); e != nil {
			t.Fatal(e)
		}
		event.WorkflowRunID = 8
		if f.assignJobInputs(event) == nil {
			t.Fatal("changed assignment accepted")
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		reopened, e := os.OpenRoot(b.root.Name())
		if e != nil {
			t.Fatal(e)
		}
		fresh := &jobInputs{root: reopened, base: b.base, policy: b.policy}
		fresh.reconcile(context.Background())
		data, _ := fresh.root.ReadFile("cursor.json")
		var n string
		json.Unmarshal(data, &n)
		if n != "" {
			seen[n] = true
		}
		fresh.root.Close()
	}
	if len(seen) != 3 {
		t.Fatalf("waiting scopes starved across restart: %v", seen)
	}
	for i := 0; i < 1001; i++ {
		if e := b.root.Mkdir(fmt.Sprintf("ci-job-old-%04d", i), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if f.prepareJobInputs(context.Background(), "ci-job-new", inputSpec()) == nil {
		t.Fatal("new admission exceeded cap")
	}
	b.reconcile(context.Background()) // Existing scopes still progress beyond cap.
	if _, e := b.root.Stat("ci-job-a/assignment.json"); e != nil {
		t.Fatal("existing scope removed")
	}
}
func TestInputAdapterCancellationAndChangedExecutable(t *testing.T) {
	_, b := inputFixture(t, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if b.invoke(ctx, "prepare", "ci-job-test") != 75 || time.Since(start) > 2*time.Second {
		t.Fatal("unbounded adapter child")
	}
	os.Chmod(b.policy.Command[0], 0755)
	os.WriteFile(b.policy.Command[0], []byte("#!/bin/sh\nexit 0\n"), 0555)
	if b.invoke(context.Background(), "prepare", "ci-job-test") != 75 {
		t.Fatal("changed executable executed")
	}
}
func TestInputFactoryIsTheSingleRunnerCreateBoundary(t *testing.T) {
	f, _ := inputFixture(t, "cat >/dev/null; exit 0")
	creates := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		if path == "/containers/create" {
			var spec obj
			json.NewDecoder(r.Body).Decode(&spec)
			if command, ok := spec["Cmd"].([]any); ok && len(command) > 0 && command[0] == "/opt/ci/runner.sh" {
				creates++
				mounts := spec["HostConfig"].(map[string]any)["Mounts"].([]any)
				found := false
				for _, v := range mounts {
					m := v.(map[string]any)
					if m["Target"] == "/run/ci-input-gate" {
						found = m["ReadOnly"] == true
					}
				}
				if !found {
					t.Error("runner created before input mount")
				}
			}
			w.WriteHeader(201)
			return
		}
		if strings.HasSuffix(path, "/wait") {
			fmt.Fprint(w, `{"StatusCode":0}`)
			return
		}
		if strings.HasPrefix(path, "/images/") {
			fmt.Fprintf(w, `{"Id":"sha256:%s"}`, strings.Repeat("a", 64))
			return
		}
		if strings.HasSuffix(path, "/json") {
			fmt.Fprintf(w, `{"NetworkSettings":{"Networks":{%q:{"IPAddress":"192.0.2.1"}}}}`, strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "-proxy/json")+"-net")
			return
		}
		if strings.HasSuffix(path, "/exec") {
			fmt.Fprint(w, `{"Id":"fixture"}`)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(201)
	})
	if e := f.start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if creates != 1 {
		t.Fatalf("runner created %d times", creates)
	}
}
