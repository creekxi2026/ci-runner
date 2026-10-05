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
	for _, owner := range owners {
		s, e := openSharedWork(root, volume, owner)
		if e != nil {
			t.Fatal(e)
		}
		f := &fleet{owner: owner, image: image, work: s, jobs: map[string]string{}}
		fleets = append(fleets, f)
		t.Cleanup(func() {
			for n, net := range f.snapshot() {
				f.cleanup(n, net)
			}
			s.Close()
		})
	}
	names := []string{}
	for i := 0; i < 3; i++ {
		f := fleets[i%2]
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
		if e := f.createContext(ctx, n, image, "1001", []string{"sleep", "120"}, nil, h, "none"); e != nil {
			t.Fatal(e)
		}
		script := `test "$(stat -f -c %T /home/runner)" != tmpfs
 test "$(stat -c %a /home/runner)" = 700
 test "$(stat -c %a /tmp)" = 1777
 test ! -e /var/lib/ci-runner/work
 test ! -e /job-disk
 test ! -e /var/run/docker.sock
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
	// Both pools are full together. A fourth start must fail before Docker/JIT.
	if e := fleets[1].start(ctx); !errors.Is(e, errCapacity) {
		t.Fatalf("fourth start: %v", e)
	}
	if pg := os.Getenv("CI_DISK_INTEGRATION_POSTGRES"); pg != "" {
		f := fleets[0]
		n := names[0]
		h := secure()
		h["ReadonlyRootfs"] = true
		h["Memory"] = 512 * 1024 * 1024
		h["MemorySwap"] = h["Memory"]
		h["Mounts"] = []obj{f.diskMount(n, "postgres", "/var/lib/postgresql/data"), f.diskMount(n, "postgres-run", "/var/run/postgresql")}
		if e := f.createContext(ctx, n+"-pg", pg, "999", []string{"postgres"}, []string{"POSTGRES_PASSWORD=fixture-only", "POSTGRES_DB=ci"}, h, "none"); e != nil {
			t.Fatal(e)
		}
		script := `for i in $(seq 1 50); do pg_isready -h 127.0.0.1 -U postgres && break; sleep .2; done
 test "$(stat -f -c %T /var/lib/postgresql/data)" != tmpfs
 psql -h 127.0.0.1 -U postgres -d ci -v ON_ERROR_STOP=1 -c 'create table isolated(n int); insert into isolated values(1);'
 test "$(psql -h 127.0.0.1 -U postgres -d ci -Atc 'select count(*) from isolated')" = 1`
		if code, e := execCode(ctx, n+"-pg", "999", []string{"sh", "-ec", script}); e != nil || code != 0 {
			t.Fatalf("PG: %d %v", code, e)
		}
	}
	// Restart discovery preserves each deployment's live jobs, including PG.
	recovered := &fleet{owner: owners[0], work: fleets[0].work}
	if e := recovered.recover(); e != nil {
		t.Fatal(e)
	}
	recovered.reap()
	if len(recovered.snapshot()) != 2 {
		t.Fatal("live jobs lost on restart")
	}
	recovered.cleanup(names[0], "")
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
	if len(restarted.snapshot()) != 1 {
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
	t.Log("two pools: global three, shared disk subpaths, worker/PG isolation, grouping, live restart, orphan cleanup, reusable slots passed")
}
