package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"
)

func additions(want, current, limit int) int { return max(0, min(limit, want)-current) }

var errMissing = errors.New("docker resource missing")

var engine = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
}}, Timeout: 120 * time.Second}

func docker(method, path string, body any, out any) error {
	return dockerContext(context.Background(), method, path, body, out)
}
func dockerContext(ctx context.Context, method, path string, body any, out any) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, &b)
	req.Header.Set("Content-Type", "application/json")
	resp, err := engine.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		if method == "DELETE" {
			return nil
		}
		return errMissing
	}
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("docker %s %s status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

type obj = map[string]any
type fleet struct {
	diagnostics          *scaleset.Diagnostics
	mu                   sync.Mutex
	client               runnerAPI
	session              listener.Client
	set                  int
	image, netout, owner string
	proxyEnv             []string
	allowedEvents        map[string]bool
	services             *serviceManager
	cleanupLog           *cleanupJournal
	cache                *dependencyCache
	incremental          *incrementalCache
	gitCache             *gitObjectCache
	seed, toolsSeed      string
	work                 *sharedWork
	jobs                 map[string]string
	scaleMu              sync.Mutex
	maxJobs              int
	states               map[string]*jobState
	messages             map[int]*messageProgress
	idle, lifetime       time.Duration
	stopping             bool
	unregister           bool
	demandSource         func(context.Context) (int, error)
	changes              chan struct{}
}

func (f *fleet) labels() obj { return obj{"ci-runner.owner": f.owner} }
func (f *fleet) create(name, image, user string, cmd, env []string, host obj, network string) error {
	return f.createContext(context.Background(), name, image, user, cmd, env, host, network)
}
func (f *fleet) createContext(ctx context.Context, name, image, user string, cmd, env []string, host obj, network string) error {
	host["NetworkMode"] = network
	host["LogConfig"] = obj{"Type": "json-file", "Config": obj{"max-size": "5m", "max-file": "2"}}
	if err := dockerContext(ctx, "POST", "/containers/create?name="+name, obj{"Image": image, "User": user, "Cmd": cmd, "Env": env, "Labels": f.containerLabels(name), "HostConfig": host}, nil); err != nil {
		return err
	}
	return dockerContext(ctx, "POST", "/containers/"+name+"/start", nil, nil)
}
func secure() obj {
	return obj{"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "PidsLimit": 512, "Memory": int64(1536 * 1024 * 1024), "NanoCpus": int64(2e9)}
}
func (f *fleet) cleanup(name, network string) {
	j := f.state(name)
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleaning = true
	f.cleanJob(name, network, j)
}
func (f *fleet) address(ctx context.Context, name, network string) (string, error) {
	var v struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	err := dockerContext(ctx, "GET", "/containers/"+name+"/json", nil, &v)
	return v.NetworkSettings.Networks[network].IPAddress, err
}
func (f *fleet) start(ctx context.Context) (err error) {
	ctx, diagnosticEnd := f.diagnostics.Begin(ctx, scaleset.Start, 1)
	defer func() {
		if err == nil {
			f.diagnostics.Count(ctx, scaleset.DiagnosticCounts{Started: 1})
		}
		diagnosticEnd(err)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, life := f.limits()
	ctx, cancel := context.WithTimeout(ctx, life)
	defer cancel()
	name := "ci-job-" + uuid.NewString()[:12]
	if f.work != nil {
		if err = f.work.reserve(name, false); err != nil {
			return err
		}
	}
	network := name + "-net"
	f.mu.Lock()
	if f.stopping || len(f.jobs) >= f.jobLimit() {
		f.mu.Unlock()
		if f.work != nil {
			_ = f.work.release(name)
		}
		return errCapacity
	}
	if f.jobs == nil {
		f.jobs = map[string]string{}
	}
	j := &jobState{created: time.Now(), gate: true, disk: true, shared: f.work != nil}
	j.mu.Lock()
	if f.states == nil {
		f.states = map[string]*jobState{}
	}
	f.states[name] = j
	f.jobs[name] = network
	f.mu.Unlock()
	defer j.mu.Unlock()
	j.created = time.Now()
	j.provisioning = true
	j.gate = true
	defer func() { j.provisioning = false }()
	released := false
	defer func() {
		if err != nil && !released {
			j.cleaning = true
			f.cleanJob(name, network, j)
		}
	}()
	if err = dockerContext(ctx, "POST", "/networks/create", obj{"Name": network, "Internal": true, "EnableIPv6": true, "Labels": f.resourceLabels(name)}, nil); err != nil {
		return
	}
	hp := secure()
	hp["ReadonlyRootfs"] = true
	hp["Memory"] = 128 * 1024 * 1024
	hp["NanoCpus"] = int64(250000000)
	if err = f.createContext(ctx, name+"-proxy", f.image, "1001", []string{"python3", "/opt/ci/egress.py"}, f.proxyEnv, hp, network); err != nil {
		return
	}
	if err = dockerContext(ctx, "POST", "/networks/"+f.netout+"/connect", obj{"Container": name + "-proxy"}, nil); err != nil {
		return
	}
	proxy, err := f.address(ctx, name+"-proxy", network)
	if err != nil {
		return err
	}
	api, set := f.api()
	if api == nil {
		return fmt.Errorf("runner API unavailable")
	}
	jit, err := api.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, set)
	if err != nil {
		return err
	}
	proxyURL := "http://" + proxy + ":3128"
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit.EncodedJITConfig, "http_proxy=" + proxyURL, "https_proxy=" + proxyURL, "HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "no_proxy=localhost,127.0.0.1", "NO_PROXY=localhost,127.0.0.1", "CI_RUNNER_IMAGE=" + f.image, "CI_TOOLS_SEED=" + f.toolsSeed}
	h := secure()
	// Listener is not an init: orphaned job descendants must be reaped after
	// cancellation/timeout, even when their original parent has already exited.
	h["Init"] = true
	h["Memory"] = 4 * 1024 * 1024 * 1024
	h["MemorySwap"] = h["Memory"]
	h["ReadonlyRootfs"] = true
	if err = f.prepareJobDisk(ctx, name, h); err != nil {
		return err
	}
	if f.services != nil {
		if _, err = f.prepareServices(name, h); err != nil {
			return err
		}
		env = append(env, "CI_SERVICES_FILE="+serviceMount+"/index.json", "CI_SERVICES_FINGERPRINT="+f.services.Fingerprint)
	}
	if err = f.prepareWorkspace(ctx, name); err != nil {
		return err
	}
	f.restoreIncremental(ctx, name)
	if err = f.prepareGitObjects(ctx, name); err != nil {
		return err
	}
	if f.seed != "" {
		env = append(env, "CI_DEPENDENCY_SEED="+f.seed)
	}
	if err = f.mountTools(h); err != nil {
		return err
	}
	env, err = f.prepareDependencyCache(ctx, name, h, env)
	if err != nil {
		return err
	}
	h["NetworkMode"] = network
	h["LogConfig"] = obj{"Type": "json-file", "Config": obj{"max-size": "5m", "max-file": "2"}}
	spec := obj{"Image": f.image, "User": "1001", "Cmd": []string{"/opt/ci/runner.sh"}, "Env": env, "Labels": f.containerLabels(name), "HostConfig": h}
	// Keep container creation and teardown under one controller.
	if err = dockerContext(ctx, "POST", "/containers/create?name="+name, spec, nil); err != nil {
		return err
	}
	if err = dockerContext(ctx, "POST", "/containers/"+name+"/start", nil, nil); err != nil {
		return
	}
	fw := secure()
	fw["CapAdd"] = []string{"NET_ADMIN"}
	fw["ReadonlyRootfs"] = true
	if err = f.createContext(ctx, name+"-fw", f.image, "0", []string{"/opt/ci/firewall.sh", proxy}, nil, fw, "container:"+name); err != nil {
		return
	}
	var result struct{ StatusCode int }
	if err = dockerContext(ctx, "POST", "/containers/"+name+"-fw/wait?condition=not-running", nil, &result); err != nil {
		return
	}
	if result.StatusCode != 0 {
		return fmt.Errorf("firewall setup failed")
	}
	if err = dockerContext(ctx, "DELETE", "/containers/"+name+"-fw?force=true", nil, nil); err != nil {
		return err
	}
	var ex struct{ ID string }
	if err = dockerContext(ctx, "POST", "/containers/"+name+"/exec", obj{"User": "1001", "Cmd": []string{"touch", "/tmp/ci-network-ready"}}, &ex); err != nil {
		return
	}
	// A lost response may still have released the runner. Preserve it for reap.
	released = true
	if err = dockerContext(ctx, "POST", "/exec/"+ex.ID+"/start", obj{"Detach": false, "Tty": false}, nil); err != nil {
		return
	}
	log.Printf("started job %s", name)
	return nil
}
func (f *fleet) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) (diagnosticErr error) {
	ctx, diagnosticEnd := f.diagnostics.Begin(ctx, scaleset.Scale, 1)
	var observation *scaleObservation
	if f.diagnostics != nil {
		observation = &scaleObservation{scaleset.DiagnosticScaleLocal{Origin: "unknown", Reason: "unknown", Handler: "unknown"}}
		if msg != nil {
			var available, started, completed uint64
			for _, e := range msg.JobAvailableMessages {
				if e != nil {
					available++
				}
			}
			for _, e := range msg.JobStartedMessages {
				if e != nil {
					started++
				}
			}
			for _, e := range msg.JobCompletedMessages {
				if e != nil {
					completed++
				}
			}
			observation.Available, observation.Started, observation.Completed = &available, &started, &completed
		}
	}
	defer func() {
		if observation != nil {
			if diagnosticErr == nil {
				observation.Origin, observation.Reason = "none", "none"
			}
			f.diagnostics.SetScaleLocal(ctx, observation.DiagnosticScaleLocal)
		}
		diagnosticEnd(diagnosticErr)
	}()
	f.scaleMu.Lock()
	defer f.scaleMu.Unlock()
	if msg == nil || msg.MessageID == listener.InitialMessageID {
		return nil
	}
	f.mu.Lock()
	stopped := f.stopping
	f.mu.Unlock()
	if stopped || ctx.Err() != nil {
		return context.Canceled
	}
	if f.messages == nil {
		f.messages = map[int]*messageProgress{}
	}
	p := f.messages[msg.MessageID]
	if p == nil {
		p = &messageProgress{}
		f.messages[msg.MessageID] = p
	}
	if p.done {
		return nil
	}
	if observation != nil {
		observation.Acquired = boolValue(p.acquired)
	}
	if !p.acquired {
		var ids []int64
		for _, j := range msg.JobAvailableMessages {
			if j != nil && f.admits(j.EventName) {
				ids = append(ids, j.RunnerRequestID)
			}
		}
		if len(ids) > 0 {
			if _, err := f.session.AcquireJobs(ctx, ids); err != nil {
				return err
			}
		}
		p.acquired = true
		if observation != nil {
			observation.Acquired = boolValue(true)
		}
	}
	// JobStarted is scheduler assignment, not proof that an offline listener
	// has run user code. Only the atomic job-start hook arbitrates idle cleanup.
	for index, event := range msg.JobStartedMessages {
		observation.event("started", index)
		if event != nil {
			if recorded, err := f.recordUnaddressableEvent(event.RunnerName, "started", event); recorded {
				if err != nil {
					return err
				}
				continue
			}
			if err := f.observeSchedulerObserved(event.RunnerName, event.RunnerID, event.JobMessageBase, "", observation); err != nil {
				return err
			}
		}
		f.changed()
		f.cacheJobEvent(event)
	}
	for index, event := range msg.JobCompletedMessages {
		if event == nil {
			continue
		}
		observation.event("completed", index)
		if recorded, err := f.recordUnaddressableEvent(event.RunnerName, "completed", event); recorded {
			if err != nil {
				return err
			}
			continue
		}
		if err := f.observeSchedulerObserved(event.RunnerName, event.RunnerID, event.JobMessageBase, event.Result, observation); err != nil {
			return err
		}
		assigned := &scaleset.JobStarted{RunnerID: event.RunnerID, RunnerName: event.RunnerName, JobMessageBase: event.JobMessageBase}
		f.changed()
		f.cacheJobEvent(assigned)
		if n, ok := f.snapshot()[event.RunnerName]; ok {
			f.cleanup(event.RunnerName, n)
			if _, pending := f.snapshot()[event.RunnerName]; pending {
				observation.fail("completion_cleanup", "completion_cleanup_pending")
				return fmt.Errorf("completion cleanup pending")
			}
		}
	}
	if observation != nil {
		observation.Handler, observation.EventIndex = "unknown", nil
		observation.NameValid, observation.StateExists, observation.JournalExists, observation.JournalSafe = nil, nil, nil, nil
	}
	if f.demandSource != nil {
		if err := f.reconcileCurrent(ctx); err != nil {
			return err
		}
	} else if msg.Statistics != nil {
		// Isolated handlers can use the supplied snapshot; run() always installs
		// the live scale-set demand source for recovery and retry reconciliation.
		for range additions(msg.Statistics.TotalAssignedJobs, len(f.snapshot()), f.jobLimit()) {
			if err := f.start(ctx); err != nil {
				return err
			}
		}
	}
	p.done = true
	return nil
}
func main() {
	if err := run(); err != nil {
		log.Print("controller stopped: ", err)
		os.Exit(1)
	}
}

type scaleSets interface {
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(context.Context, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

func ensureScaleSet(ctx context.Context, c scaleSets, name string) (*scaleset.RunnerScaleSet, error) {
	existing, err := c.GetRunnerScaleSet(ctx, 1, name)
	if err != nil {
		return nil, err
	}
	desired := &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: false}}
	if existing == nil {
		return c.CreateRunnerScaleSet(ctx, desired)
	}
	if existing.RunnerSetting.DisableUpdate {
		return c.UpdateRunnerScaleSet(ctx, existing.ID, desired)
	}
	return existing, nil
}

func run() (err error) {
	// Refuse retired policy before touching credentials, Docker or GitHub.
	if os.Getenv("JOB_INPUT_POLICY_FILE") != "" {
		return fmt.Errorf("JOB_INPUT_POLICY_FILE is retired; drain and migrate the protected pool before upgrading")
	}
	diagnostics := openDiagnostics(os.Getenv("CI_DIAGNOSTIC_PROFILE"), os.Stdout)
	defer closeDiagnostics(diagnostics)
	_, initEnd := diagnostics.Begin(context.Background(), scaleset.InitConfig, 1)
	defer func() {
		if initEnd != nil {
			initEnd(err)
		}
	}()
	maxJobs, err := poolJobLimit(os.Getenv("RUNNER_MAX_JOBS"))
	if err != nil {
		return err
	}
	events, err := allowedEvents(os.Getenv("RUNNER_ALLOWED_EVENTS"))
	if err != nil {
		return err
	}
	cache, err := dependencyCacheConfig(os.Getenv("DEPENDENCY_CACHE_MODE"), os.Getenv("DEPENDENCY_CACHE_TRUST_LANE"), os.Getenv("GITHUB_CONFIG_URL"), os.Getenv("DEPLOYMENT_ID"), events, os.Getenv("DEPENDENCY_CACHE_VOLUME"))
	if err != nil {
		return err
	}
	toolsSeed := os.Getenv("TOOLS_SEED")
	if toolsSeed != "" && (cache != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(toolsSeed)) {
		return fmt.Errorf("tools seed requires exact SHA256 and shared writable cache off")
	}
	seed := os.Getenv("DEPENDENCY_CACHE_SEED")
	if seed != "" && (cache != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(seed)) {
		return fmt.Errorf("dependency seed requires exact SHA256 and shared writable cache off")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	initEnd(nil)
	_, initEnd = diagnostics.Begin(ctx, scaleset.InitCredentials, 1)
	token, err := os.ReadFile("/run/secrets/github_token")
	if err != nil {
		return err
	}
	initEnd(nil)
	_, initEnd = diagnostics.Begin(ctx, scaleset.InitClient, 1)
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: os.Getenv("GITHUB_CONFIG_URL"), PersonalAccessToken: string(bytes.TrimSpace(token))}, scaleset.WithDiagnostics(diagnostics))
	if err != nil {
		return err
	}
	runnerImage, controllerImage, err := deploymentImages(ctx)
	if err != nil {
		return err
	}
	if err = verifyControllerImage(ctx, controllerImage); err != nil {
		return err
	}
	proxyEnv, err := upstreamProxyEnv(os.Getenv("PUBLIC_EGRESS_UPSTREAM_PROXY"))
	if err != nil {
		return err
	}
	dohEnv, err := dohProxyEnv(os.Getenv("PUBLIC_EGRESS_DOH_URL"))
	if err != nil {
		return err
	}
	proxyEnv = append(proxyEnv, dohEnv...)
	f := &fleet{diagnostics: diagnostics, maxJobs: maxJobs, cache: cache, seed: seed, toolsSeed: toolsSeed, client: c, allowedEvents: events, proxyEnv: proxyEnv, image: runnerImage, netout: os.Getenv("EGRESS_NETWORK"), owner: os.Getenv("DEPLOYMENT_ID"), jobs: map[string]string{}, unregister: true}
	if f.owner == "" {
		return fmt.Errorf("DEPLOYMENT_ID required")
	}
	f.services, err = loadServices(ctx, os.Getenv("SERVICES_CATALOG_FILE"), os.Getenv("SERVICES_POLICY_FILE"), controllerImage, runnerImage, toolsSeed, seed)
	if err != nil {
		return err
	}
	f.idle, f.lifetime, err = timeoutConfig()
	if err != nil {
		return err
	}
	initEnd(nil)
	_, initEnd = diagnostics.Begin(ctx, scaleset.InitDisk, 1)
	f.work, err = openSharedWork(os.Getenv("JOB_WORK_ROOT"), os.Getenv("JOB_WORK_VOLUME"), f.owner, maxJobs)
	if err != nil {
		return err
	}
	defer f.work.Close()
	if err = f.verifyWorkMount(ctx); err != nil {
		return err
	}
	if mode := os.Getenv("JOB_CLEANUP_JOURNAL"); mode != "" && mode != "off" {
		if mode != "on" {
			return fmt.Errorf("JOB_CLEANUP_JOURNAL must be on or off")
		}
		f.cleanupLog, err = openCleanupJournal(f.work, 0)
		if err != nil {
			return err
		}
		defer f.cleanupLog.root.Close()
	}
	if err = f.openIncrementalCache(ctx, os.Getenv("INCREMENTAL_CACHE_POLICY_FILE"), string(bytes.TrimSpace(token)), os.Getenv("GITHUB_CONFIG_URL")); err != nil {
		return err
	}
	if f.incremental != nil {
		defer f.incremental.root.Close()
	}
	if err = f.openGitObjectCache(ctx, os.Getenv("GIT_OBJECT_CACHE_MODE"), string(bytes.TrimSpace(token)), os.Getenv("GITHUB_CONFIG_URL"), os.Getenv("GIT_OBJECT_CACHE_REFS")); err != nil {
		return err
	}
	initEnd(nil)
	initEnd = nil
	f.changes = make(chan struct{}, 1)
	f.demandSource = scaleSetDemand(c, os.Getenv("SCALE_SET_NAME"), func() int { _, id := f.api(); return id })
	// Recovery and the local janitor start before registration/session acquisition.
	if err = f.recover(); err != nil {
		return err
	}
	if err = f.recoverCleanupObservations(); err != nil {
		return err
	}
	localCtx, stopLocal := context.WithCancel(context.Background())
	defer stopLocal()
	localDone := make(chan struct{})
	go func() { defer close(localDone); f.maintain(localCtx, 10*time.Second) }()
	coordinatorDone := make(chan struct{})
	go func() { defer close(coordinatorDone); f.coordinate(ctx, 10*time.Second) }()
	var diagnosticAttempt uint64
	retry(ctx, time.Second, func() error {
		diagnosticAttempt++
		callCtx, end := diagnostics.Begin(ctx, scaleset.EnsureSet, diagnosticAttempt)
		set, e := ensureScaleSet(callCtx, c, os.Getenv("SCALE_SET_NAME"))
		end(e)
		if e != nil {
			return e
		}
		f.mu.Lock()
		f.set = set.ID
		f.mu.Unlock()
		session, e := c.MessageSessionClient(ctx, set.ID, "ci-runner-controller")
		if e != nil {
			return e
		}
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = session.Close(closeCtx)
		}()
		f.session = session
		return f.listen(ctx, time.Second)
	})
	log.Print("shutdown: scheduling stopped; draining until jobs exit or reach lifetime")
	drained := f.drain(time.Second)
	stopLocal()
	for _, done := range []chan struct{}{localDone, coordinatorDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			return fmt.Errorf("shutdown: local I/O still pending; labeled resources retained for recovery")
		}
	}
	if !drained {
		return fmt.Errorf("drain deadline reached; remaining resources require recovery")
	}
	return nil
}
