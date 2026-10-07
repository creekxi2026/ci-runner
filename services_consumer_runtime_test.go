package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The consuming project supplies catalog.json, policy.json and a probe.py which
// supports route/acquire/isolation/recover modes. No product name, DB engine,
// credential schema or SQL is built into this harness or the controller.
func TestServiceConsumerRuntime(t *testing.T) {
	if os.Getenv("CI_SERVICE_CONSUMER_TEST") == "" {
		t.Skip("explicit project fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	runner, controller, e := deploymentImages(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = verifyControllerImage(ctx, controller); e != nil {
		t.Fatal(e)
	}
	manager, e := loadServices(ctx, "/consumer/catalog.json", "/consumer/policy.json", controller, runner, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if len(manager.Catalog.Services) != 1 {
		t.Fatal("consumer fixture requires one declared service")
	}
	id := ""
	for k := range manager.Catalog.Services {
		id = k
	}
	work, e := openSharedWork(os.Getenv("CI_SERVICE_TEST_ROOT"), os.Getenv("CI_SERVICE_TEST_VOLUME"), os.Getenv("CI_SERVICE_TEST_OWNER"), 2)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(work.Close)
	f := &fleet{owner: work.owner, image: runner, work: work, services: manager, jobs: map[string]string{}, lifetime: 2 * time.Minute}
	if e = f.verifyWorkMount(ctx); e != nil {
		t.Fatal(e)
	}
	names := []string{"ci-job-" + uuid.NewString()[:12], "ci-job-" + uuid.NewString()[:12]}
	t.Cleanup(func() {
		for _, n := range names {
			f.cleanup(n, f.snapshot()[n])
		}
	})
	probe := func(n, phase, other string) (int, error) {
		return execCode(ctx, n, "1001", []string{"python3", "/consumer/probe.py", phase, other})
	}
	for _, n := range names {
		if e = work.reserve(n, false); e != nil {
			t.Fatal(e)
		}
		f.jobs[n] = n + "-net"
		j := f.state(n)
		j.created = time.Now()
		j.disk = true
		j.shared = true
		if e = dockerContext(ctx, "POST", "/networks/create", obj{"Name": n + "-net", "Internal": true, "Labels": f.resourceLabels(n)}, nil); e != nil {
			t.Fatal(e)
		}
		h := secure()
		h["ReadonlyRootfs"] = true
		if e = f.prepareJobDisk(ctx, n, h); e != nil {
			t.Fatal(e)
		}
		if _, e = f.prepareServices(n, h); e != nil {
			t.Fatal(e)
		}
		env := []string{"CI_SERVICES_FILE=" + serviceMount + "/index.json", "CI_SERVICES_FINGERPRINT=" + manager.Fingerprint, "CI_RUNNER_IMAGE=" + runner, "TEST_CONTROLLER_IMAGE=" + controller}
		if e = f.createContext(ctx, n, runner, "1001", []string{"sleep", "180"}, env, h, n+"-net"); e != nil {
			t.Fatal(e)
		}
		fw := secure()
		fw["ReadonlyRootfs"] = true
		fw["CapAdd"] = []string{"NET_ADMIN"}
		if e = f.createContext(ctx, n+"-fw", runner, "0", []string{"/bin/sh", "/opt/ci/firewall.sh", "192.0.2.1"}, nil, fw, "container:"+n); e != nil {
			t.Fatal(e)
		}
		if e = f.serviceWait(ctx, n+"-fw"); e != nil {
			t.Fatal(e)
		}
		if e = dockerContext(ctx, "DELETE", "/containers/"+n+"-fw?force=true", nil, nil); e != nil {
			t.Fatal(e)
		}
		if code, e := probe(n, "route", ""); e != nil || code != 0 {
			t.Fatalf("consumer route status=%d error=%v", code, e)
		}
		var c serviceContainer
		if e = docker("GET", "/containers/"+serviceName(n, id, "server")+"/json", nil, &c); !errors.Is(e, errMissing) {
			t.Fatal("routing started a service")
		}
	}
	maintainCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.maintain(maintainCtx, 250*time.Millisecond) }()
	stopAndJoin := func() { stop(); <-done }
	defer stopAndJoin()
	// Actual project CLI requests drive the normal controller maintenance loop.
	var wg sync.WaitGroup
	failures := make(chan string, 2)
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, e := probe(n, "acquire", ""); e != nil || code != 0 {
				failures <- n
			}
		}()
	}
	wg.Wait()
	close(failures)
	for n := range failures {
		t.Errorf("consumer acquisition/validation rejected for %s (credential diagnostics suppressed)", n)
	}
	if t.Failed() {
		return
	}
	first, e := f.readServiceIndex(names[0])
	if e != nil {
		t.Fatal(e)
	}
	second, e := f.readServiceIndex(names[1])
	if e != nil {
		t.Fatal(e)
	}
	if first.Lease == second.Lease || first.Fingerprint != second.Fingerprint {
		t.Fatal("random lease/static fingerprint binding incorrect")
	}
	records := []serviceRecord{}
	for _, n := range names {
		r, e := f.readServiceRecord(n, id)
		if e != nil || r.Phase != "ready" {
			t.Fatal("missing ready state")
		}
		records = append(records, r)
	}
	for i, n := range names {
		if code, e := probe(n, "isolation", records[1-i].Endpoint.Host); e != nil || code != 0 {
			t.Fatalf("consumer isolation status=%d error=%v", code, e)
		}
	}
	stopAndJoin()
	recovered := &fleet{owner: f.owner, image: runner, work: work, services: manager, lifetime: f.lifetime}
	if e = recovered.recover(); e != nil {
		t.Fatal(e)
	}
	recovered.reap()
	for i, n := range names {
		j := recovered.state(n)
		if j == nil {
			t.Fatal("lost live consumer job")
		}
		recovered.provisionService(ctx, n, id, j.created)
		r, e := recovered.readServiceRecord(n, id)
		if e != nil || r.Endpoint.Host != records[i].Endpoint.Host || !r.Deadline.Equal(records[i].Deadline) {
			t.Fatal("recovery changed endpoint/deadline")
		}
		if code, e := probe(n, "recover", ""); e != nil || code != 0 {
			t.Fatalf("consumer recovery status=%d error=%v", code, e)
		}
	}
	recovered.cleanup(names[0], names[0]+"-net")
	if _, ok := recovered.snapshot()[names[0]]; ok {
		t.Fatal("consumer cancellation leaked")
	}
	if e = docker("DELETE", "/containers/"+names[1]+"?force=true", nil, nil); e != nil {
		t.Fatal(e)
	}
	orphan := &fleet{owner: f.owner, image: runner, work: work, services: manager}
	if e = orphan.recover(); e != nil {
		t.Fatal(e)
	}
	orphan.reap()
	if len(orphan.snapshot()) != 0 {
		t.Fatal("consumer orphan cleanup pending")
	}
	for _, n := range names {
		var v obj
		if e = docker("GET", "/networks/"+serviceName(n, id, "net"), nil, &v); !errors.Is(e, errMissing) {
			t.Fatal("consumer network leaked")
		}
	}
	f.jobs = map[string]string{}
	t.Log("project consumer: genuine local manifest admission, computed fingerprint, route without allocation, parallel CLI acquisition, strict consumer validation, isolation, stable recovery, cancellation and orphan cleanup passed")
}
