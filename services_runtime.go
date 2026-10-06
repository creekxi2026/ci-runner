package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type serviceTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// The job mutex protects this map, but is never held by provisioning workers.
// Cleanup cancels their Docker I/O before joining them, so readiness cannot
// prevent cancellation/expiry from being observed by the janitor.
func (f *fleet) pollServices(parent context.Context, n string) {
	j := f.state(n)
	if j == nil || !j.mu.TryLock() {
		return
	}
	defer j.mu.Unlock()
	if j.cleaning || j.provisioning || j.created.IsZero() {
		return
	}
	if j.services == nil {
		j.services = map[string]*serviceTask{}
	}
	for id := range f.services.Catalog.Services {
		if task := j.services[id]; task != nil {
			select {
			case <-task.done:
				delete(j.services, id)
			default:
				continue
			}
		}
		ctx, cancel := context.WithCancel(parent)
		task := &serviceTask{cancel: cancel, done: make(chan struct{})}
		j.services[id] = task
		created := j.created
		go func() { defer close(task.done); defer cancel(); f.provisionService(ctx, n, id, created) }()
	}
}
func (f *fleet) cancelServices(j *jobState) {
	for _, t := range j.services {
		t.cancel()
	}
	for _, t := range j.services {
		<-t.done
	}
	j.services = nil
}
func (f *fleet) provisionService(parent context.Context, n, id string, created time.Time) {
	// Query only a fixed, validated request path. The worker can request an
	// allowlisted ID, not pass a command or catalog through the control channel.
	probe, cancel := context.WithTimeout(parent, 5*time.Second)
	code, e := execCode(probe, n, "1001", []string{"test", "-e", "/tmp/ci-service-request-" + id})
	cancel()
	if e != nil || code != 0 {
		return
	}
	index, e := f.readServiceIndex(n)
	if e != nil {
		f.serviceFailure(parent, n, id)
		return
	}
	spec := f.services.Catalog.Services[id]
	r, e := f.readServiceRecord(n, id)
	if errors.Is(e, os.ErrNotExist) {
		_, life := f.limits()
		r = serviceRecord{Phase: "starting", Deadline: minTime(time.Now().Add(time.Duration(spec.Resources.Startup)*time.Second), created.Add(life))}
		if e = f.saveServiceRecord(n, id, r); e != nil {
			f.serviceFailure(parent, n, id)
			return
		}
	} else if e != nil {
		f.serviceFailure(parent, n, id)
		return
	}
	if r.Phase == "failed" {
		f.serviceFailure(parent, n, id)
		return
	}
	if r.Phase == "ready" {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if f.verifyServiceContainer(ctx, index, id, "server", spec.Image, true) != nil || f.verifyServiceContainer(ctx, index, id, "ns", f.image, true) != nil {
			r.Phase = "failed"
			_ = f.saveServiceRecord(n, id, r)
			f.serviceFailure(parent, n, id)
			_ = f.removeOneService(index, id)
		}
		return
	}
	ctx, cancel := context.WithDeadline(parent, r.Deadline)
	defer cancel()
	if ctx.Err() == nil {
		e = f.startService(ctx, index, id, spec, &r)
	} else {
		e = ctx.Err()
	}
	if e != nil {
		// A controller shutdown preserves the durable phase for recovery. Job
		// cleanup separately cancels then removes resources. A real init failure
		// is terminal and cannot be retried into a fresh instance on the same lease.
		if parent.Err() != nil {
			return
		}
		r.Phase = "failed"
		_ = f.saveServiceRecord(n, id, r)
		f.serviceFailure(parent, n, id)
		_ = f.removeOneService(index, id)
	}
}
func (f *fleet) serviceFailure(parent context.Context, n, id string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_, _ = execCode(ctx, n, "1001", []string{"touch", "/tmp/ci-service-failed-" + id})
}
func (f *fleet) serviceLabels(index *serviceIndex, id, role string) obj {
	l := f.resourceLabels(index.Job)
	l["ci-runner.service"] = id
	l["ci-runner.service-role"] = role
	l["ci-runner.lease"] = index.Lease
	l["ci-runner.fingerprint"] = index.Fingerprint
	l["com.docker.compose.project"] = "ci-jobs"
	l["com.docker.compose.service"] = serviceName(index.Job, id, role)
	return l
}
func serviceMatches(labels map[string]string, index *serviceIndex, id, role string) bool {
	return labels["ci-runner.owner"] == index.Owner && labels["ci-runner.job"] == index.Job && labels["ci-runner.service"] == id && labels["ci-runner.service-role"] == role && labels["ci-runner.lease"] == index.Lease && labels["ci-runner.fingerprint"] == index.Fingerprint
}

type serviceContainer struct {
	State struct {
		Running bool
		Status  string
	}
	Config struct {
		Image  string
		Labels map[string]string
	}
}

func (f *fleet) verifyServiceContainer(ctx context.Context, index *serviceIndex, id, role, image string, running bool) error {
	var c serviceContainer
	if e := dockerContext(ctx, "GET", "/containers/"+serviceName(index.Job, id, role)+"/json", nil, &c); e != nil {
		return e
	}
	if !serviceMatches(c.Config.Labels, index, id, role) || c.Config.Image != image || (running && !c.State.Running) {
		return fmt.Errorf("service container identity/state mismatch")
	}
	return nil
}
func (f *fleet) serviceCreate(ctx context.Context, index *serviceIndex, id, role, image, user string, entry, cmd, env []string, h obj, network string) error {
	name := serviceName(index.Job, id, role)
	h["NetworkMode"] = network
	h["LogConfig"] = obj{"Type": "none"}
	h["RestartPolicy"] = obj{"Name": "no"}
	c := obj{"Image": image, "Labels": f.serviceLabels(index, id, role), "HostConfig": h}
	if user != "" {
		c["User"] = user
	}
	if entry != nil {
		c["Entrypoint"] = entry
	}
	if cmd != nil {
		c["Cmd"] = cmd
	}
	if env != nil {
		c["Env"] = env
	}
	if e := dockerContext(ctx, "POST", "/containers/create?name="+name, c, nil); e != nil {
		return e
	}
	return dockerContext(ctx, "POST", "/containers/"+name+"/start", nil, nil)
}
func (f *fleet) serviceWait(ctx context.Context, name string) error {
	var r struct{ StatusCode *int }
	if e := dockerContext(ctx, "POST", "/containers/"+name+"/wait?condition=not-running", nil, &r); e != nil {
		return e
	}
	if r.StatusCode == nil || *r.StatusCode != 0 {
		return fmt.Errorf("service phase failed")
	}
	return nil
}
func serviceHost(spec serviceSpec, fraction int64) obj {
	h := secure()
	r := spec.Resources
	h["ReadonlyRootfs"] = true
	h["Memory"] = r.Memory * fraction / 16
	h["MemorySwap"] = h["Memory"]
	h["NanoCpus"] = r.CPU * 1000000 * fraction / 16
	h["PidsLimit"] = r.Pids * fraction / 16
	h["Tmpfs"] = obj{"/tmp": "rw,nosuid,nodev,noexec,size=16m,mode=1777", "/run": "rw,nosuid,nodev,noexec,size=16m,mode=755"}
	return h
}
func (f *fleet) startService(ctx context.Context, index *serviceIndex, id string, spec serviceSpec, r *serviceRecord) error {
	n := index.Job
	network := serviceName(n, id, "net")
	ns := serviceName(n, id, "ns")
	var netState struct {
		Internal bool
		Labels   map[string]string
	}
	e := dockerContext(ctx, "GET", "/networks/"+network, nil, &netState)
	if errors.Is(e, errMissing) {
		e = dockerContext(ctx, "POST", "/networks/create", obj{"Name": network, "Internal": true, "EnableIPv6": false, "Labels": f.serviceLabels(index, id, "net")}, nil)
	} else if e == nil && (!netState.Internal || !serviceMatches(netState.Labels, index, id, "net")) {
		e = fmt.Errorf("foreign service network")
	}
	if e != nil {
		return e
	}
	// Do not treat a duplicate connect as success without verifying membership.
	workerIP, e := f.address(ctx, n, network)
	if e != nil {
		return e
	}
	if workerIP == "" {
		if e = dockerContext(ctx, "POST", "/networks/"+network+"/connect", obj{"Container": n}, nil); e != nil {
			return e
		}
		workerIP, e = f.address(ctx, n, network)
	}
	if e != nil || net.ParseIP(workerIP) == nil {
		return fmt.Errorf("missing worker service address")
	}
	e = f.verifyServiceContainer(ctx, index, id, "ns", f.image, true)
	if errors.Is(e, errMissing) {
		h := serviceHost(spec, 1)
		e = f.serviceCreate(ctx, index, id, "ns", f.image, "1001", []string{"/bin/sleep"}, []string{"infinity"}, nil, h, network)
	}
	if e != nil {
		return e
	}
	host, e := f.address(ctx, ns, network)
	if e != nil || net.ParseIP(host) == nil {
		return fmt.Errorf("missing service address")
	}
	if r.Endpoint.Host != "" && r.Endpoint.Host != host {
		return fmt.Errorf("service endpoint changed")
	}
	r.Endpoint = serviceEndpoint{host, spec.Ports}
	if e = f.saveServiceRecord(n, id, *r); e != nil {
		return e
	}
	// Install the firewall before the service or adapter process can run. Only
	// the requesting worker may initiate connections to declared ports. Init and
	// check share this private namespace and reach the service locally.
	args := []string{"/opt/ci/service-firewall.sh", "server", workerIP}
	for _, p := range spec.Ports {
		args = append(args, strconv.Itoa(p))
	}
	if e = f.serviceFirewall(ctx, index, id, "guard", args, "container:"+ns); e != nil {
		return e
	}
	e = f.verifyServiceContainer(ctx, index, id, "server", spec.Image, true)
	if errors.Is(e, errMissing) {
		h := serviceHost(spec, 12)
		// A pinned service entrypoint may need to drop root to its image's own user.
		// These filesystem/UID capabilities never grant network or host administration.
		h["CapAdd"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID"}
		tmp := h["Tmpfs"].(obj)
		for _, target := range spec.Storage {
			tmp[target] = fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d,mode=755", spec.Resources.Data/int64(len(spec.Storage)))
		}
		h["Mounts"] = []obj{f.serviceVolumeMount(serviceDir(n, id)+"/secrets-service", "/run/ci-service-secrets", true)}
		env := []string{}
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env)
		e = f.serviceCreate(ctx, index, id, "server", spec.Image, "", nil, nil, env, h, "container:"+ns)
	}
	if e != nil {
		return e
	} // Never restart a stopped tmpfs service or reset its lease.
	if e = f.runServiceAdapter(ctx, index, id, spec, r.Endpoint, "init"); e != nil {
		return e
	}
	if e = f.runServiceAdapter(ctx, index, id, spec, r.Endpoint, "check"); e != nil {
		return e
	}
	if e = f.verifyServiceContainer(ctx, index, id, "server", spec.Image, true); e != nil {
		return e
	}
	args = []string{"/opt/ci/service-firewall.sh", "worker", host}
	for _, p := range spec.Ports {
		args = append(args, strconv.Itoa(p))
	}
	if e = f.serviceFirewall(ctx, index, id, "allow", args, "container:"+n); e != nil {
		return e
	}
	if e = f.publishService(index, id, r.Endpoint); e != nil {
		return e
	}
	r.Phase = "ready"
	return f.saveServiceRecord(n, id, *r)
}
func (f *fleet) serviceFirewall(ctx context.Context, index *serviceIndex, id, role string, args []string, network string) error {
	if e := f.removeServiceContainer(ctx, index, id, role); e != nil {
		return e
	}
	h := serviceHost(f.services.Catalog.Services[id], 1)
	h["CapAdd"] = []string{"NET_ADMIN"}
	if e := f.serviceCreate(ctx, index, id, role, f.image, "0", []string{"/bin/sh"}, args, nil, h, network); e != nil {
		return e
	}
	if e := f.serviceWait(ctx, serviceName(index.Job, id, role)); e != nil {
		return e
	}
	return f.removeServiceContainer(ctx, index, id, role)
}
func (f *fleet) runServiceAdapter(ctx context.Context, index *serviceIndex, id string, spec serviceSpec, endpoint serviceEndpoint, phase string) error {
	n := index.Job
	if e := f.removeServiceContainer(ctx, index, id, phase); e != nil {
		return e
	}
	secretPaths := map[string]string{}
	mounts := []obj{f.serviceVolumeMount(serviceDir(n, id)+"/"+phase, "/run/ci-adapter", true), f.serviceVolumeMount(serviceDir(n, id)+"/outputs", "/outputs", phase == "check")}
	if phase == "init" {
		mounts = append(mounts, f.serviceVolumeMount(serviceDir(n, id)+"/secrets-init", "/run/ci-service-secrets", true))
		for secret, recipients := range spec.Secrets {
			for _, recipient := range recipients {
				if recipient == "init" {
					secretPaths[secret] = "/run/ci-service-secrets/" + secret
				}
			}
		}
	}
	request := obj{"version": 1, "lease_id": index.Lease, "owner_id": index.Owner, "job_id": n, "service_id": id, "endpoint": endpoint, "config": spec.Config, "secret_files": secretPaths}
	if e := f.serviceWriteJSON(serviceDir(n, id)+"/"+phase+"/request.json", request, 0444); e != nil {
		return e
	}
	h := serviceHost(spec, 2)
	h["Mounts"] = mounts
	if e := f.serviceCreate(ctx, index, id, phase, spec.Adapter, "0", []string{"/ci-adapter"}, []string{phase}, nil, h, "container:"+serviceName(n, id, "ns")); e != nil {
		return e
	}
	if e := f.serviceWait(ctx, serviceName(n, id, phase)); e != nil {
		return e
	}
	return f.removeServiceContainer(ctx, index, id, phase)
}
func (f *fleet) removeServiceContainer(ctx context.Context, index *serviceIndex, id, role string) error {
	var c serviceContainer
	name := serviceName(index.Job, id, role)
	e := dockerContext(ctx, "GET", "/containers/"+name+"/json", nil, &c)
	if errors.Is(e, errMissing) {
		return nil
	}
	if e != nil {
		return e
	}
	if !serviceMatches(c.Config.Labels, index, id, role) {
		return fmt.Errorf("refusing foreign service cleanup")
	}
	return dockerContext(ctx, "DELETE", "/containers/"+name+"?force=true&v=true", nil, nil)
}
func (f *fleet) removeOneService(index *serviceIndex, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, role := range []string{"init", "check", "guard", "allow", "server", "ns"} {
		if e := f.removeServiceContainer(ctx, index, id, role); e != nil {
			return e
		}
	}
	network := serviceName(index.Job, id, "net")
	var v struct {
		Labels     map[string]string
		Containers map[string]struct{ Name string }
	}
	e := dockerContext(ctx, "GET", "/networks/"+network, nil, &v)
	if errors.Is(e, errMissing) {
		return nil
	}
	if e != nil {
		return e
	}
	if !serviceMatches(v.Labels, index, id, "net") {
		return fmt.Errorf("refusing foreign service network cleanup")
	}
	for _, c := range v.Containers {
		if c.Name != index.Job {
			return fmt.Errorf("foreign service network endpoint")
		}
	}
	if len(v.Containers) > 0 {
		if e = dockerContext(ctx, "POST", "/networks/"+network+"/disconnect", obj{"Container": index.Job, "Force": true}, nil); e != nil {
			return e
		}
	}
	return dockerContext(ctx, "DELETE", "/networks/"+network, nil, nil)
}

// Cleanup uses the persisted identity, even if an operator changed the catalog.
// Configuration drift must prevent reuse without preventing owned cleanup.
func (f *fleet) removeServices(n string) error {
	if f.work == nil {
		return nil
	}
	b, e := f.serviceRead(serviceBase(n)+"/index.json", 65536)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var index serviceIndex
	if e = strictServiceDecode(b, &index); e != nil {
		return e
	}
	if index.Owner != f.owner || index.Job != n || !serviceHex.MatchString(index.Lease) {
		return fmt.Errorf("foreign service cleanup lease")
	}
	for id := range index.Services {
		if !serviceID.MatchString(id) {
			return fmt.Errorf("invalid service cleanup ID")
		}
		if e = f.removeOneService(&index, id); e != nil {
			return e
		}
	}
	return nil
}

// Names are only a routing hint. All adoption/deletion additionally checks the
// controller's persisted owner/job/lease/fingerprint labels.
func genericServiceJob(name string) string {
	if i := strings.Index(name, "-svc-"); i > 0 && validJobName.MatchString(name[:i]) {
		return name[:i]
	}
	return ""
}
