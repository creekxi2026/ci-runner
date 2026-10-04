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
	"sync"
	"syscall"
	"time"
)

func additions(want, current int) int { return max(0, min(3, want)-current) }

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
	mu                            sync.Mutex
	client                        runnerAPI
	session                       listener.Client
	set                           int
	image, pgImage, netout, owner string
	proxyEnv                      []string
	jobs                          map[string]string
	scaleMu                       sync.Mutex
	states                        map[string]*jobState
	messages                      map[int]*messageProgress
	idle, lifetime                time.Duration
	stopping                      bool
	unregister                    bool
	demandSource                  func(context.Context) (int, error)
	changes                       chan struct{}
}

func (f *fleet) labels() obj { return obj{"ci-runner.owner": f.owner} }
func (f *fleet) create(name, image, user string, cmd, env []string, host obj, network string) error {
	return f.createContext(context.Background(), name, image, user, cmd, env, host, network)
}
func (f *fleet) createContext(ctx context.Context, name, image, user string, cmd, env []string, host obj, network string) error {
	host["NetworkMode"] = network
	host["LogConfig"] = obj{"Type": "json-file", "Config": obj{"max-size": "5m", "max-file": "2"}}
	if err := dockerContext(ctx, "POST", "/containers/create?name="+name, obj{"Image": image, "User": user, "Cmd": cmd, "Env": env, "Labels": f.resourceLabels(jobName(name)), "HostConfig": host}, nil); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	_, life := f.limits()
	ctx, cancel := context.WithTimeout(ctx, life)
	defer cancel()
	name := "ci-job-" + uuid.NewString()[:12]
	network := name + "-net"
	f.mu.Lock()
	if f.stopping || len(f.jobs) >= 3 {
		f.mu.Unlock()
		return fmt.Errorf("scheduling stopped or capacity full")
	}
	if f.jobs == nil {
		f.jobs = map[string]string{}
	}
	j := &jobState{created: time.Now(), gate: true}
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
	pg := secure()
	pg["Tmpfs"] = obj{"/var/lib/postgresql/data": "rw,size=512m", "/var/run/postgresql": "rw,size=16m"}
	pg["ReadonlyRootfs"] = true
	pg["Memory"] = 512 * 1024 * 1024
	pg["NanoCpus"] = int64(500000000)
	if err = f.createContext(ctx, name+"-pg", f.postgresImage(), "999", []string{"postgres"}, []string{"POSTGRES_PASSWORD=ci-disposable", "POSTGRES_DB=ci", "PGDATA=/var/lib/postgresql/data/pgdata"}, pg, network); err != nil {
		return
	}
	database, err := f.address(ctx, name+"-pg", network)
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
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit.EncodedJITConfig, "http_proxy=" + proxyURL, "https_proxy=" + proxyURL, "HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "no_proxy=localhost,127.0.0.1," + database, "NO_PROXY=localhost,127.0.0.1," + database, "DATABASE_URL=postgres://postgres:ci-disposable@" + database + ":5432/ci?sslmode=disable", "CI_DATABASE_HOST=" + database}
	h := secure()
	h["Memory"] = 1536 * 1024 * 1024
	h["ReadonlyRootfs"] = true
	h["Tmpfs"] = obj{"/home/runner": "rw,exec,size=2g,nr_inodes=262144,uid=1001,gid=1001,mode=0700", "/tmp": "rw,exec,size=128m,nr_inodes=32768,mode=1777"}
	if err = f.createContext(ctx, name, f.image, "1001", []string{"/opt/ci/runner.sh"}, env, h, network); err != nil {
		return
	}
	fw := secure()
	fw["CapAdd"] = []string{"NET_ADMIN"}
	fw["ReadonlyRootfs"] = true
	if err = f.createContext(ctx, name+"-fw", f.image, "0", []string{"/opt/ci/firewall.sh", proxy, database}, nil, fw, "container:"+name); err != nil {
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
func (f *fleet) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
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
	if !p.acquired {
		var ids []int64
		for _, j := range msg.JobAvailableMessages {
			if j.EventName == "workflow_dispatch" {
				ids = append(ids, j.RunnerRequestID)
			}
		}
		if len(ids) > 0 {
			if _, err := f.session.AcquireJobs(ctx, ids); err != nil {
				return err
			}
		}
		p.acquired = true
	}
	for _, event := range msg.JobStartedMessages {
		if _, ok := f.snapshot()[event.RunnerName]; ok {
			j := f.state(event.RunnerName)
			if j == nil {
				continue
			}
			j.mu.Lock()
			j.busy = true
			j.mu.Unlock()
		}
	}
	for _, event := range msg.JobCompletedMessages {
		if n, ok := f.snapshot()[event.RunnerName]; ok {
			f.cleanup(event.RunnerName, n)
			if _, pending := f.snapshot()[event.RunnerName]; pending {
				return fmt.Errorf("completion cleanup pending")
			}
		}
	}
	if f.demandSource != nil {
		if err := f.reconcileCurrent(ctx); err != nil {
			return err
		}
	} else if msg.Statistics != nil {
		// Isolated handlers can use the supplied snapshot; run() always installs
		// the live scale-set demand source for recovery and retry reconciliation.
		for range additions(msg.Statistics.TotalAssignedJobs, len(f.snapshot())) {
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

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	token, err := os.ReadFile("/run/secrets/github_token")
	if err != nil {
		return err
	}
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: os.Getenv("GITHUB_CONFIG_URL"), PersonalAccessToken: string(bytes.TrimSpace(token))})
	if err != nil {
		return err
	}
	runnerImage, postgresImage, err := deploymentImages(ctx)
	if err != nil {
		return err
	}
	proxyEnv, err := upstreamProxyEnv(os.Getenv("PUBLIC_EGRESS_UPSTREAM_PROXY"))
	if err != nil {
		return err
	}
	f := &fleet{client: c, proxyEnv: proxyEnv, image: runnerImage, pgImage: postgresImage, netout: os.Getenv("EGRESS_NETWORK"), owner: os.Getenv("DEPLOYMENT_ID"), jobs: map[string]string{}, unregister: true}
	if f.owner == "" {
		return fmt.Errorf("DEPLOYMENT_ID required")
	}
	f.idle, f.lifetime, err = timeoutConfig()
	if err != nil {
		return err
	}
	f.changes = make(chan struct{}, 1)
	f.demandSource = scaleSetDemand(c, os.Getenv("SCALE_SET_NAME"), func() int { _, id := f.api(); return id })
	// Recovery and the local janitor start before registration/session acquisition.
	if err = f.recover(); err != nil {
		return err
	}
	localCtx, stopLocal := context.WithCancel(context.Background())
	defer stopLocal()
	localDone := make(chan struct{})
	go func() { defer close(localDone); f.maintain(localCtx, 10*time.Second) }()
	coordinatorDone := make(chan struct{})
	go func() { defer close(coordinatorDone); f.coordinate(ctx, 10*time.Second) }()
	retry(ctx, time.Second, func() error {
		set, e := ensureScaleSet(ctx, c, os.Getenv("SCALE_SET_NAME"))
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
