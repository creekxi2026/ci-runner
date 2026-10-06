package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Run inside a root test controller with the same local disk volume mounted at
// CI_WORK_TEST_ROOT and Docker socket. No production pool or cache is touched.
func TestSharedWorkRuntime(t *testing.T) {
	root := os.Getenv("CI_WORK_TEST_ROOT")
	if root == "" {
		t.Skip("requires isolated Docker work volume")
	}
	volume := os.Getenv("CI_WORK_TEST_VOLUME")
	image := os.Getenv("CI_DISK_INTEGRATION_IMAGE")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	prefix := os.Getenv("CI_WORK_TEST_OWNER_PREFIX")
	if prefix == "" {
		prefix = fmt.Sprintf("work-fixture-%d", time.Now().UnixNano())
	}
	owners := []string{prefix + "-a", prefix + "-b"}
	fleets := []*fleet{}
	for i, owner := range owners {
		limit := []int{3, 1}[i]
		s, e := openSharedWork(root, volume, owner, limit)
		if e != nil {
			t.Fatal(e)
		}
		f := &fleet{maxJobs: limit, owner: owner, image: image, work: s, jobs: map[string]string{}}
		fleets = append(fleets, f)
		t.Cleanup(func() {
			for n, net := range f.snapshot() {
				f.cleanup(n, net)
			}
			s.Close()
		})
	}
	names := []string{}
	for i := 0; i < 4; i++ {
		pool := 0
		if i == 1 {
			pool = 1
		}
		f := fleets[pool]
		n := fmt.Sprintf("ci-job-%08x-%03x", uint32(time.Now().UnixNano()), i)
		names = append(names, n)
		if e := f.work.reserve(n, false); e != nil {
			t.Fatal(e)
		}
		f.jobs[n] = ""
		j := f.state(n)
		j.disk = true
		j.shared = true
		h := secure()
		h["ReadonlyRootfs"] = true
		h["Memory"] = 256 * 1024 * 1024
		h["MemorySwap"] = h["Memory"]
		if e := f.prepareJobDisk(ctx, n, h); e != nil {
			t.Fatal(e)
		}
		if e := f.prepareWorkspace(ctx, n); e != nil {
			t.Fatal(e)
		}
		if e := f.createContext(ctx, n, image, "1001", []string{"sleep", "120"}, nil, h, "none"); e != nil {
			t.Fatal(e)
		}
		script := `test "$(stat -f -c %T /home/runner)" != tmpfs
 test "$(stat -c %a /home/runner)" = 700
 test "$(stat -c %a /tmp)" = 1777
 test ! -e /var/lib/ci-runner/work
 test ! -e /job-disk
 test ! -e /var/run/docker.sock
 test ! -e /templates
 test -s /home/runner/.ci-workspace-ready
 test -x /home/runner/bin/Runner.Worker
 test -x /home/runner/externals/node24/bin/npm
 printf 'private modification' > /home/runner/config.sh
 test ! -e /home/runner/private
 printf private > /home/runner/private
 ln -s /etc /home/runner/escape
 mkdir /home/runner/unreadable; chmod 000 /home/runner/unreadable`
		if i == 0 {
			script += "\nfallocate -l 2300M /home/runner/disk-test; rm /home/runner/disk-test"
		}
		if code, e := execCode(ctx, n, "1001", []string{"sh", "-ec", script}); e != nil || code != 0 {
			t.Fatalf("job isolation: %d %v", code, e)
		}
		var c struct {
			Config struct{ Labels map[string]string }
			Mounts []struct{ Name, Destination string }
		}
		if e := docker("GET", "/containers/"+n+"/json", nil, &c); e != nil {
			t.Fatal(e)
		}
		if c.Config.Labels["com.docker.compose.project"] != "ci-jobs" {
			t.Fatal("job not grouped")
		}
		for _, m := range c.Mounts {
			if m.Name != volume {
				t.Fatal("per-job volume still created")
			}
		}
	}
	// The one-job pool is full; the other pool still has independent capacity.
	if e := fleets[1].start(ctx); !errors.Is(e, errCapacity) {
		t.Fatalf("self-test pool exceeded one: %v", e)
	}
	// Restart discovery preserves each deployment's live jobs.
	recovered := &fleet{owner: owners[0], work: fleets[0].work}
	if e := recovered.recover(); e != nil {
		t.Fatal(e)
	}
	recovered.reap()
	if len(recovered.snapshot()) != 3 {
		t.Fatal("live jobs lost on restart")
	}
	recovered.cleanup(names[0], "")
	fleets[1].cleanup(names[1], "")
	if _, ok := recovered.snapshot()[names[0]]; ok {
		t.Fatal("job cleanup retained")
	}
	// New pool can claim the freed global slot; its directory has no old data.
	n := "ci-job-ffffffff-123"
	f := fleets[1]
	if e := f.work.reserve(n, false); e != nil {
		t.Fatal(e)
	}
	if e := f.work.prepare(n); e != nil {
		t.Fatal(e)
	}
	if _, e := f.work.root.Stat("jobs/" + n + "/home/private"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("old task data reused")
	}
	// Simulate a crash between directory creation and Docker provisioning.
	restarted := &fleet{owner: owners[1], work: f.work}
	if e := restarted.recover(); e != nil {
		t.Fatal(e)
	}
	restarted.reap()
	if _, e := f.work.root.Stat("jobs/" + n); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("directory-only orphan leaked")
	}
	if len(restarted.snapshot()) != 0 {
		t.Fatal("foreign/live task removed")
	}
	for _, fl := range fleets {
		for n, net := range fl.snapshot() {
			fl.cleanup(n, net)
		}
	}
	all, e := f.work.leases()
	if e != nil || len(all) != 0 {
		t.Fatalf("leases leaked: %v %v", all, e)
	}
	if _, e := f.work.root.Stat("schema"); e != nil {
		t.Fatal("shared volume removed")
	}
	t.Log("two pools: independent three and one, shared disk subpaths, worker isolation, grouping, live restart, orphan cleanup, reusable slots passed")
}
