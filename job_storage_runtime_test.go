package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Uses only task-owned resources, without GitHub registration or shared caches.
func TestJobDiskRuntime(t *testing.T) {
	image := os.Getenv("CI_DISK_INTEGRATION_IMAGE")
	if image == "" {
		t.Skip("set CI_DISK_INTEGRATION_IMAGE for real Docker disk lifecycle checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := &fleet{owner: fmt.Sprintf("disk-check-%d", time.Now().UnixNano()), image: image, jobs: map[string]string{}}
	names := []string{}
	for i := 0; i < 2; i++ {
		n := fmt.Sprintf("ci-job-%08x-%03x", uint32(time.Now().UnixNano()), i)
		names = append(names, n)
		f.jobs[n] = ""
		f.state(n).disk = true
		t.Cleanup(func() {
			f.cleanup(n, "")
			if _, retained := f.snapshot()[n]; retained {
				t.Errorf("fixture cleanup retained %v", f.snapshot())
			}
		})
		h := secure()
		h["ReadonlyRootfs"] = true
		h["Memory"] = 256 * 1024 * 1024
		h["MemorySwap"] = h["Memory"]
		if err := f.prepareJobDisk(ctx, n, h); err != nil {
			t.Fatal(err)
		}
		if err := f.createContext(ctx, n, image, "1001:1001", []string{"sleep", "120"}, nil, h, "none"); err != nil {
			t.Fatal(err)
		}
		script := `test "$(stat -f -c %T /home/runner)" != tmpfs
test "$(stat -f -c %T /tmp)" != tmpfs
test "$(stat -c %a /home/runner)" = 700
test "$(stat -c %a /tmp)" = 1777
test ! -e /home/runner/other-job
test ! -w /opt/ci
test ! -e /var/run/docker.sock
printf private > /home/runner/other-job
printf '#!/bin/sh\nexit 0\n' > /tmp/executable; chmod +x /tmp/executable; /tmp/executable
test ! -e /job-disk
test ! -e /postgres
test ! -e /opt/ci-cache`
		if i == 0 {
			// Allocate more than the previous 2 GiB HOME ceiling while the
			// runner has only 256 MiB RAM; disk allocation must still succeed.
			script += "\nfallocate -l 2300M /home/runner/disk-capacity; test $(stat -c %s /home/runner/disk-capacity) -eq 2411724800; rm /home/runner/disk-capacity"
		}
		if code, err := execCode(ctx, n, "1001", []string{"sh", "-ec", script}); err != nil || code != 0 {
			t.Fatalf("private disk check: code=%d error=%v", code, err)
		}
	}
	if pgImage := os.Getenv("CI_DISK_INTEGRATION_POSTGRES"); pgImage != "" {
		n := names[0]
		h := secure()
		h["ReadonlyRootfs"] = true
		h["Memory"] = 512 * 1024 * 1024
		h["MemorySwap"] = h["Memory"]
		h["Mounts"] = []obj{jobDiskMount(n, "postgres", "/var/lib/postgresql/data"), jobDiskMount(n, "postgres-run", "/var/run/postgresql")}
		env := []string{"POSTGRES_PASSWORD=local-fixture-only", "POSTGRES_DB=ci", "PGDATA=/var/lib/postgresql/data/pgdata"}
		if err := f.createContext(ctx, n+"-pg", pgImage, "999", []string{"postgres"}, env, h, "none"); err != nil {
			t.Fatal(err)
		}
		script := `for i in $(seq 1 50); do pg_isready -h 127.0.0.1 -U postgres && break; sleep 0.2; done
test "$(stat -f -c %T /var/lib/postgresql/data)" != tmpfs
test ! -e /home/runner/other-job
psql -U postgres -d ci -v ON_ERROR_STOP=1 -c 'CREATE TABLE disk_test (n int); INSERT INTO disk_test VALUES (1);'
test "$(psql -U postgres -d ci -Atc 'SELECT count(*) FROM disk_test')" = 1`
		if code, err := execCode(ctx, n+"-pg", "999", []string{"sh", "-ec", script}); err != nil || code != 0 {
			t.Fatalf("PostgreSQL disk check: code=%d error=%v", code, err)
		}
	}
	// A controller restart must preserve both live runners and their disk volumes.
	recovered := &fleet{owner: f.owner}
	if err := recovered.recover(); err != nil {
		t.Fatal(err)
	}
	if len(recovered.jobs) != 2 {
		t.Fatal("lost live disk jobs")
	}
	recovered.reap()
	if len(recovered.jobs) != 2 {
		t.Fatal("reaped live disks")
	}
	// Normal cancellation removes the runner before its private disk.
	recovered.cleanup(names[0], "")
	var v diskVolume
	if err := docker("GET", "/volumes/"+jobVolume(names[0]), nil, &v); !errors.Is(err, errMissing) {
		t.Fatalf("canceled disk leaked: %v", err)
	}
	// Simulate a crash after container removal but before disk removal.
	if err := docker("DELETE", "/containers/"+names[1]+"?force=true", nil, nil); err != nil {
		t.Fatal(err)
	}
	restarted := &fleet{owner: f.owner}
	if err := restarted.recover(); err != nil {
		t.Fatal(err)
	}
	restarted.reap()
	if len(restarted.jobs) != 0 {
		t.Fatal("orphan disk not reclaimed after restart")
	}
	if err := docker("GET", "/volumes/"+jobVolume(names[1]), nil, &v); !errors.Is(err, errMissing) {
		t.Fatalf("orphan disk leaked: %v", err)
	}
	t.Log("disk-backed private HOME/tmp, permissions, isolation, live recovery, cancellation and orphan cleanup passed")
}
