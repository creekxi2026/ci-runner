package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The fixture builds locally from an already pulled ARM64 runner. Image IDs
// deliberately bypass the registry admission layer (covered separately); all
// container/network/file/provisioning/cleanup operations use real Docker.
func TestServiceRuntime(t *testing.T) {
	image := os.Getenv("CI_SERVICE_TEST_IMAGE")
	if image == "" {
		t.Skip("run scripts/check-service-runtime.sh for real isolated Docker acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 210*time.Second)
	defer cancel()
	owner := os.Getenv("CI_SERVICE_TEST_OWNER")
	work, e := openSharedWork(os.Getenv("CI_SERVICE_TEST_ROOT"), os.Getenv("CI_SERVICE_TEST_VOLUME"), owner, 3)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(work.Close)
	catalog := serviceCatalog{1, map[string]serviceSpec{}}
	for id, mode := range map[string]string{"echo": "ok", "broken": "fail", "slow": "resume", "unsafe": "symlink"} {
		catalog.Services[id] = serviceSpec{Image: image, Adapter: image, Config: map[string]any{"mode": mode}, Ports: []int{8765}, Resources: serviceResources{536870912, 1000, 128, 16777216, 30}, Storage: []string{"/data"}, Env: map[string]string{}, Secrets: map[string][]string{"bootstrap": {"init"}}}
	}
	f := &fleet{owner: owner, image: image, work: work, services: &serviceManager{catalog, strings.Repeat("f", 64)}, jobs: map[string]string{}, lifetime: 3 * time.Minute}
	names := []string{"ci-job-" + uuid.NewString()[:12], "ci-job-" + uuid.NewString()[:12]}
	t.Cleanup(func() {
		for _, n := range names {
			f.cleanup(n, f.snapshot()[n])
		}
	})
	for _, n := range names {
		if e = work.reserve(n, false); e != nil {
			t.Fatal(e)
		}
		f.jobs[n] = n + "-net"
		j := f.state(n)
		j.created = time.Now()
		j.shared = true
		j.disk = true
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
		env := []string{"CI_SERVICES_FILE=" + serviceMount + "/index.json", "CI_SERVICES_FINGERPRINT=" + f.services.Fingerprint}
		if e = f.createContext(ctx, n, image, "1001", []string{"sleep", "240"}, env, h, n+"-net"); e != nil {
			t.Fatal(e)
		}
		fw := secure()
		fw["ReadonlyRootfs"] = true
		fw["CapAdd"] = []string{"NET_ADMIN"}
		if e = f.createContext(ctx, n+"-fw", image, "0", []string{"/bin/sh", "/opt/ci/firewall.sh", "192.0.2.1"}, nil, fw, "container:"+n); e != nil {
			t.Fatal(e)
		}
		if e = f.serviceWait(ctx, n+"-fw"); e != nil {
			t.Fatal(e)
		}
		if e = dockerContext(ctx, "DELETE", "/containers/"+n+"-fw?force=true", nil, nil); e != nil {
			t.Fatal(e)
		}
	}
	request := func(n, id string) {
		t.Helper()
		if code, e := execCode(ctx, n, "1001", []string{"touch", "/tmp/ci-service-request-" + id}); e != nil || code != 0 {
			t.Fatalf("request %s: %d %v", id, code, e)
		}
	}
	var wg sync.WaitGroup
	for _, n := range names {
		request(n, "echo")
		wg.Add(1)
		go func() { defer wg.Done(); f.provisionService(ctx, n, "echo", f.state(n).created) }()
	}
	wg.Wait()
	addresses := []string{}
	leases := []string{}
	for _, n := range names {
		r, e := f.readServiceRecord(n, "echo")
		if e != nil || r.Phase != "ready" {
			t.Fatalf("echo not ready: %+v %v", r, e)
		}
		addresses = append(addresses, r.Endpoint.Host)
		index, e := f.readServiceIndex(n)
		if e != nil {
			t.Fatal(e)
		}
		leases = append(leases, index.Lease)
		// Both acquire and exec validate publication in the real read-only worker
		// mount. No mocks of UID, permissions or statvfs are used here.
		code, e := execCode(ctx, n, "1001", []string{"/usr/local/bin/ci-service", "exec", "echo", "--timeout", "2", "--", "python3", "-c", `import json,os,stat,socket
from pathlib import Path
root=Path('/run/ci-services')
assert os.statvfs(root).f_flag & os.ST_RDONLY
for p in [root,root/'services',root/'services/echo',root/'services/echo/outputs']:
 s=p.stat(); assert s.st_uid==0 and stat.S_IMODE(s.st_mode)==0o755
for p in [root/'index.json',root/'services/echo/ready.json']:
 s=p.stat(); assert s.st_uid==0 and stat.S_IMODE(s.st_mode)==0o444 and s.st_nlink==1
p=root/'services/echo/outputs/consumer.json'; s=p.stat()
assert s.st_uid==1001 and stat.S_IMODE(s.st_mode)==0o400 and s.st_nlink==1
assert not os.access(p,os.W_OK)
assert not Path('/run/ci-service-secrets').exists()
assert not Path('/var/run/docker.sock').exists()
assert not Path('/work').exists()
r=json.loads(Path(os.environ['CI_SERVICE_FILE']).read_text())
with socket.create_connection((r['endpoint']['host'],8765),timeout=2) as c:
 c.sendall(b'own-service'); assert c.recv(1024)==b'own-service'
`})
		if e != nil || code != 0 {
			t.Fatalf("publication/CLI: %d %v", code, e)
		}
		for _, role := range []string{"server", "ns"} {
			var c struct {
				HostConfig struct {
					LogConfig                               struct{ Type string }
					Memory, MemorySwap, NanoCpus, PidsLimit int64
				}
			}
			if e = docker("GET", "/containers/"+serviceName(n, "echo", role)+"/json", nil, &c); e != nil {
				t.Fatal(e)
			}
			if c.HostConfig.LogConfig.Type != "none" || c.HostConfig.Memory != c.HostConfig.MemorySwap || c.HostConfig.Memory <= 0 || c.HostConfig.NanoCpus <= 0 || c.HostConfig.PidsLimit <= 0 {
				t.Fatal("service logs or resource limits unsafe")
			}
		}
		// Repeated requests retain the same immutable identity and running instance.
		f.provisionService(ctx, n, "echo", f.state(n).created)
		again, e := f.readServiceIndex(n)
		if e != nil || again.Lease != index.Lease {
			t.Fatal("duplicate request replaced lease")
		}
	}
	if leases[0] == leases[1] || addresses[0] == addresses[1] {
		t.Fatal("jobs share service identity")
	}
	// The service namespace itself cannot initiate host/public/sibling traffic;
	// blocking only the worker would leave a compromised service as an escape.
	for i, n := range names {
		code, e := execCode(ctx, serviceName(n, "echo", "server"), "0", []string{"python3", "-c", fmt.Sprintf(`import socket
for host,port in [(%q,8765),('1.1.1.1',443),('169.254.169.254',80),('127.0.0.11',53)]:
 s=socket.socket();s.settimeout(.2)
 try:s.connect((host,port))
 except OSError:pass
 else:raise AssertionError('service egress escaped')
 finally:s.close()`, addresses[1-i])})
		if e != nil || code != 0 {
			t.Fatalf("service egress: %d %v", code, e)
		}
	}
	for i, n := range names {
		code, e := execCode(ctx, n, "1001", []string{"python3", "-c", fmt.Sprintf(`import socket
s=socket.socket();s.settimeout(.4)
try:s.connect((%q,8765))
except OSError:pass
else:raise AssertionError('sibling reachable')`, addresses[1-i])})
		if e != nil || code != 0 {
			t.Fatalf("sibling isolation: %d %v", code, e)
		}
	}
	code, e := execCode(ctx, names[0], "1001", []string{"/usr/local/bin/ci-service", "ready", "unsafe", "--timeout", "1"})
	if e != nil || code != 69 {
		t.Fatalf("unrequested ready: %d %v", code, e)
	}
	code, e = execCode(ctx, names[0], "1001", []string{"/usr/local/bin/ci-service", "acquire", "unsafe", "--timeout", "1"})
	if e != nil || code != 124 {
		t.Fatalf("bounded acquire wait: %d %v", code, e)
	}
	for _, id := range []string{"broken", "unsafe"} {
		request(names[0], id)
		f.provisionService(ctx, names[0], id, f.state(names[0]).created)
		r, e := f.readServiceRecord(names[0], id)
		if e != nil || r.Phase != "failed" {
			t.Fatalf("%s did not fail closed", id)
		}
		f.provisionService(ctx, names[0], id, f.state(names[0]).created)
		var c serviceContainer
		if e = docker("GET", "/containers/"+serviceName(names[0], id, "server")+"/json", nil, &c); !errors.Is(e, errMissing) {
			t.Fatal("failed service retried or leaked")
		}
		if _, e = f.work.root.Stat(servicePublic(names[0]) + "/services/" + id + "/ready.json"); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("failed service published ready")
		}
	}
	// A slow initializer cannot hold the job mutex and delay cancellation.
	request(names[0], "slow")
	f.pollServices(ctx, names[0])
	deadline := time.Now().Add(10 * time.Second)
	for {
		var c serviceContainer
		e = docker("GET", "/containers/"+serviceName(names[0], "slow", "init")+"/json", nil, &c)
		if e == nil && c.State.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow init never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	started := time.Now()
	f.cleanup(names[0], names[0]+"-net")
	if time.Since(started) > 10*time.Second {
		t.Fatal("cleanup waited for init deadline")
	}
	if _, ok := f.snapshot()[names[0]]; ok {
		t.Fatal("canceled job leaked")
	}
	// Interrupt the controller, not the job, during initialization. The same
	// owned live service and adapter checkpoint must resume with the same lease.
	request(names[1], "slow")
	interrupted, stopController := context.WithCancel(ctx)
	defer stopController()
	f.pollServices(interrupted, names[1])
	deadline = time.Now().Add(10 * time.Second)
	for {
		b, e := f.serviceRead(serviceDir(names[1], "slow")+"/outputs/consumer.json", 65536)
		if e == nil && strings.Contains(string(b), "checkpoint") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery checkpoint not reached")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopController()
	state := f.state(names[1])
	state.mu.Lock()
	f.cancelServices(state)
	state.mu.Unlock()
	beforeResume, e := f.readServiceRecord(names[1], "slow")
	if e != nil || beforeResume.Phase != "starting" {
		t.Fatal("interrupted initialization lost durable phase")
	}
	// Recover the remaining live job using durable index and service labels.
	recovered := &fleet{owner: owner, image: image, work: work, services: f.services, lifetime: f.lifetime}
	if e = recovered.recover(); e != nil {
		t.Fatal(e)
	}
	if len(recovered.snapshot()) != 1 {
		t.Fatalf("recovery jobs=%v", recovered.snapshot())
	}
	recovered.reap()
	if len(recovered.snapshot()) != 1 {
		t.Fatal("live job lost during recovery")
	}
	recovered.provisionService(ctx, names[1], "echo", recovered.state(names[1]).created)
	recovered.provisionService(ctx, names[1], "slow", recovered.state(names[1]).created)
	resumed, e := recovered.readServiceRecord(names[1], "slow")
	if e != nil || resumed.Phase != "ready" || resumed.Endpoint.Host != beforeResume.Endpoint.Host || resumed.Deadline != beforeResume.Deadline {
		t.Fatalf("initialization recovery reset identity/deadline: %+v %v", resumed, e)
	}
	index, e := recovered.readServiceIndex(names[1])
	if e != nil || index.Lease != leases[1] {
		t.Fatal("recovery changed identity")
	}
	// A controller can recover and reclaim service namespaces after worker loss.
	if e = docker("DELETE", "/containers/"+names[1]+"?force=true", nil, nil); e != nil {
		t.Fatal(e)
	}
	orphan := &fleet{owner: owner, image: image, work: work, services: f.services}
	if e = orphan.recover(); e != nil {
		t.Fatal(e)
	}
	orphan.reap()
	if len(orphan.snapshot()) != 0 {
		t.Fatal("orphan service not reclaimed")
	}
	for _, n := range names {
		for id := range catalog.Services {
			var netState obj
			if e = docker("GET", "/networks/"+serviceName(n, id, "net"), nil, &netState); !errors.Is(e, errMissing) {
				t.Fatal("service network leaked")
			}
		}
	}
	// Avoid double cleanup through stale test-only fleet maps after recovery.
	f.jobs = map[string]string{}
	t.Log("real Docker: concurrent isolation, read-only UID/modes, CLI, duplicate acquire, failure secrecy, terminal failure, cancellation, recovery and orphan cleanup passed")
}
