package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func additions(want, current int) int { return max(0, min(3, want)-current) }

var engine = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
}}, Timeout: 120 * time.Second}

func docker(method, path string, body any, out any) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, _ := http.NewRequest(method, "http://docker/v1.47"+path, &b)
	req.Header.Set("Content-Type", "application/json")
	resp, err := engine.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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
	mu                   sync.Mutex
	client               *scaleset.Client
	session              listener.Client
	set                  int
	image, netout, owner string
	jobs                 map[string]string
}

func (f *fleet) labels() obj { return obj{"ci-runner.owner": f.owner} }
func (f *fleet) create(name, image, user string, cmd, env []string, host obj, network string) error {
	host["NetworkMode"] = network
	host["LogConfig"] = obj{"Type": "json-file", "Config": obj{"max-size": "5m", "max-file": "2"}}
	if err := docker("POST", "/containers/create?name="+name, obj{"Image": image, "User": user, "Cmd": cmd, "Env": env, "Labels": f.labels(), "HostConfig": host}, nil); err != nil {
		return err
	}
	return docker("POST", "/containers/"+name+"/start", nil, nil)
}
func secure() obj {
	return obj{"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "PidsLimit": 512, "Memory": int64(1536 * 1024 * 1024), "NanoCpus": int64(2e9)}
}
func (f *fleet) cleanup(name, network string) {
	for _, n := range []string{name + "-fw", name, name + "-pg", name + "-proxy"} {
		_ = docker("DELETE", "/containers/"+n+"?force=true&v=true", nil, nil)
	}
	_ = docker("DELETE", "/networks/"+network, nil, nil)
	delete(f.jobs, name)
	log.Printf("removed job %s", name)
}
func (f *fleet) address(name, network string) (string, error) {
	var v struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	err := docker("GET", "/containers/"+name+"/json", nil, &v)
	return v.NetworkSettings.Networks[network].IPAddress, err
}
func (f *fleet) start(ctx context.Context) (err error) {
	name := "ci-job-" + uuid.NewString()[:12]
	network := name + "-net"
	defer func() {
		if err != nil {
			f.cleanup(name, network)
		}
	}()
	if err = docker("POST", "/networks/create", obj{"Name": network, "Internal": true, "EnableIPv6": true, "Labels": f.labels()}, nil); err != nil {
		return
	}
	hp := secure()
	hp["ReadonlyRootfs"] = true
	hp["Memory"] = 256 * 1024 * 1024
	if err = f.create(name+"-proxy", f.image, "1001", []string{"python3", "/opt/ci/egress.py"}, nil, hp, network); err != nil {
		return
	}
	if err = docker("POST", "/networks/"+f.netout+"/connect", obj{"Container": name + "-proxy"}, nil); err != nil {
		return
	}
	proxy, err := f.address(name+"-proxy", network)
	if err != nil {
		return err
	}
	pg := secure()
	pg["Tmpfs"] = obj{"/var/lib/postgresql/data": "rw,size=512m", "/var/run/postgresql": "rw,size=16m"}
	pg["ReadonlyRootfs"] = true
	pg["Memory"] = 768 * 1024 * 1024
	if err = f.create(name+"-pg", "postgres:17-bookworm", "999", []string{"postgres"}, []string{"POSTGRES_PASSWORD=ci-disposable", "POSTGRES_DB=ci", "PGDATA=/var/lib/postgresql/data/pgdata"}, pg, network); err != nil {
		return
	}
	database, err := f.address(name+"-pg", network)
	if err != nil {
		return err
	}
	jit, err := f.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, f.set)
	if err != nil {
		return err
	}
	proxyURL := "http://" + proxy + ":3128"
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit.EncodedJITConfig, "http_proxy=" + proxyURL, "https_proxy=" + proxyURL, "HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "no_proxy=localhost,127.0.0.1," + database, "NO_PROXY=localhost,127.0.0.1," + database, "DATABASE_URL=postgres://postgres:ci-disposable@" + database + ":5432/ci?sslmode=disable", "CI_DATABASE_HOST=" + database}
	h := secure()
	h["Memory"] = 2 * 1024 * 1024 * 1024
	if err = f.create(name, f.image, "1001", []string{"/opt/ci/runner.sh"}, env, h, network); err != nil {
		return
	}
	fw := secure()
	fw["CapAdd"] = []string{"NET_ADMIN"}
	fw["ReadonlyRootfs"] = true
	if err = f.create(name+"-fw", f.image, "0", []string{"/opt/ci/firewall.sh", proxy, database}, nil, fw, "container:"+name); err != nil {
		return
	}
	var result struct{ StatusCode int }
	if err = docker("POST", "/containers/"+name+"-fw/wait?condition=not-running", nil, &result); err != nil {
		return
	}
	if result.StatusCode != 0 {
		return fmt.Errorf("firewall setup failed")
	}
	_ = docker("DELETE", "/containers/"+name+"-fw?force=true", nil, nil)
	var ex struct{ ID string }
	if err = docker("POST", "/containers/"+name+"/exec", obj{"User": "1001", "Cmd": []string{"touch", "/tmp/ci-network-ready"}}, &ex); err != nil {
		return
	}
	if err = docker("POST", "/exec/"+ex.ID+"/start", obj{"Detach": false, "Tty": false}, nil); err != nil {
		return
	}
	f.jobs[name] = network
	log.Printf("started job %s capacity=%d", name, len(f.jobs))
	return nil
}
func (f *fleet) reap() {
	for name, n := range f.jobs {
		var v struct{ State struct{ Running bool } }
		if err := docker("GET", "/containers/"+name+"/json", nil, &v); err == nil && !v.State.Running {
			f.cleanup(name, n)
		}
	}
}
func (f *fleet) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reap()
	if msg == nil {
		return nil
	}
	var ids []int64
	for _, j := range msg.JobAvailableMessages {
		// Only manually dispatched jobs can enter this pre-audit deployment.
		// No PR, pull_request_target, workflow_run or public fork execution.
		if j.EventName == "workflow_dispatch" {
			ids = append(ids, j.RunnerRequestID)
		} else {
			log.Printf("rejected event %s", j.EventName)
		}
	}
	if len(ids) > 0 {
		if _, err := f.session.AcquireJobs(ctx, ids); err != nil {
			return err
		}
	}
	for _, j := range msg.JobCompletedMessages {
		if n, ok := f.jobs[j.RunnerName]; ok {
			f.cleanup(j.RunnerName, n)
		}
	}
	if msg.Statistics != nil {
		for range additions(msg.Statistics.TotalAssignedJobs, len(f.jobs)) {
			if err := f.start(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		log.Print("controller stopped: ", err)
		os.Exit(1)
	}
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
	set, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: os.Getenv("SCALE_SET_NAME"), RunnerGroupID: 1, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: false}})
	if err != nil {
		return fmt.Errorf("scale set registration failed: %w", err)
	}
	session, err := c.MessageSessionClient(ctx, set.ID, "ci-runner-controller")
	if err != nil {
		return err
	}
	defer session.Close(context.Background())
	f := &fleet{client: c, session: session, set: set.ID, image: os.Getenv("RUNNER_IMAGE"), netout: os.Getenv("EGRESS_NETWORK"), owner: os.Getenv("DEPLOYMENT_ID"), jobs: map[string]string{}}
	if f.owner == "" {
		return fmt.Errorf("DEPLOYMENT_ID required")
	}
	// Recover only resources belonging to this exact deployment after crash.
	var containers []struct{ Names []string }
	filter, _ := json.Marshal(obj{"label": []string{"ci-runner.owner=" + f.owner}})
	if err = docker("GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filter)), nil, &containers); err != nil {
		return err
	}
	for _, v := range containers {
		for _, n := range v.Names {
			_ = docker("DELETE", "/containers/"+n[1:]+"?force=true&v=true", nil, nil)
		}
	}
	var networks []struct{ ID string }
	if err = docker("GET", "/networks?filters="+url.QueryEscape(string(filter)), nil, &networks); err != nil {
		return err
	}
	for _, v := range networks {
		_ = docker("DELETE", "/networks/"+v.ID, nil, nil)
	}
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for n, v := range f.jobs {
			f.cleanup(n, v)
		}
	}()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				f.mu.Lock()
				f.reap()
				f.mu.Unlock()
			}
		}
	}()
	l, err := listener.New(session, listener.Config{ScaleSetID: set.ID, MaxRunners: 3})
	if err != nil {
		return err
	}
	log.Printf("listening scale-set=%s id=%d max=3", os.Getenv("SCALE_SET_NAME"), set.ID)
	return l.Run(ctx, f)
}
