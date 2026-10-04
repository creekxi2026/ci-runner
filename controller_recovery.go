package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/actions/scaleset"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type runnerAPI interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}
type jobState struct {
	mu                           sync.Mutex
	created                      time.Time
	busy, cleaning, provisioning bool
	resources                    []string
	recovered                    bool
	gate                         bool
}
type messageProgress struct {
	acquired, done bool
}

func jobName(n string) string {
	for _, suffix := range []string{"-proxy", "-pg", "-fw", "-net"} {
		n = strings.TrimSuffix(n, suffix)
	}
	return n
}
func (f *fleet) resourceLabels(n string) obj {
	l := f.labels()
	l["ci-runner.job"] = n
	l["ci-runner.idle-gate"] = "1"
	return l
}
func (f *fleet) state(n string) *jobState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.jobs[n]; !exists {
		return nil
	}
	if f.states == nil {
		f.states = map[string]*jobState{}
	}
	if f.states[n] == nil {
		f.states[n] = &jobState{}
	}
	return f.states[n]
}
func (f *fleet) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]string{}
	for n, v := range f.jobs {
		m[n] = v
	}
	return m
}
func (f *fleet) api() (runnerAPI, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.client, f.set }

// Caller holds only this job's mutex. Keep the runner and companions together
// if removing the runner fails; never tear its database out from under it.
func (f *fleet) cleanJob(n, network string, j *jobState) {
	resources := []string{n, n + "-fw", n + "-pg", n + "-proxy"}
	if j.recovered {
		resources = j.resources
	}
	for _, r := range resources {
		if err := docker("DELETE", "/containers/"+r+"?force=true&v=true", nil, nil); err != nil {
			return
		}
	}
	// Retain the network as a recovery tombstone until safe deregistration succeeds.
	if f.unregister {
		api, set := f.api()
		if api == nil || set == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		runner, err := api.GetRunnerByName(ctx, n)
		if err != nil {
			return
		}
		if runner != nil {
			if runner.Name != n || runner.RunnerScaleSetID != set {
				return
			}
			if err = api.RemoveRunner(ctx, int64(runner.ID)); err != nil && !errors.Is(err, scaleset.RunnerNotFoundError) && !errors.Is(err, scaleset.NotFoundError) {
				return
			}
		}
	}
	if network != "" {
		if err := docker("DELETE", "/networks/"+network, nil, nil); err != nil {
			return
		}
	}
	f.mu.Lock()
	delete(f.jobs, n)
	delete(f.states, n)
	f.mu.Unlock()
	f.changed()
}

type containerState struct {
	Created time.Time
	State   struct {
		Running   bool
		StartedAt time.Time
	}
	Config struct{ Labels map[string]string }
}

func (f *fleet) reap() {
	for n, network := range f.snapshot() {
		j := f.state(n)
		if j == nil {
			continue
		}
		if !j.mu.TryLock() {
			continue
		}
		func() {
			defer j.mu.Unlock()
			if j.cleaning {
				f.cleanJob(n, network, j)
				return
			}
			var v containerState
			err := docker("GET", "/containers/"+n+"/json", nil, &v)
			if errors.Is(err, errMissing) || (err == nil && !v.State.Running) {
				j.cleaning = true
				f.cleanJob(n, network, j)
				return
			}
			if err == nil && !v.Created.IsZero() && (j.created.IsZero() || v.Created.Before(j.created)) {
				j.created = v.Created
			}
			idle, life := f.limits()
			if !j.created.IsZero() && time.Since(j.created) >= life {
				j.cleaning = true
				f.cleanJob(n, network, j)
				return
			}
			// Failed inspect, legacy runner, or a claimed gate are ambiguous: preserve
			// work until the immutable Docker creation deadline, never trust statistics.
			if err != nil || j.created.IsZero() || j.busy || time.Since(j.created) < idle {
				return
			}
			if !j.gate && v.Config.Labels["ci-runner.idle-gate"] != "1" {
				return
			}
			if f.claimIdle(n) {
				j.cleaning = true
				f.cleanJob(n, network, j)
			}
		}()
	}
}

// The job-start hook competes for the same atomic mkdir. Once we win, the hook
// must fail before user steps run. A job can forge/claim this path, so it only
// postpones idle cleanup; it never extends the controller's absolute lifetime.
func (f *fleet) claimIdle(n string) bool {
	var exec struct{ ID string }
	if docker("POST", "/containers/"+n+"/exec", obj{"User": "1001", "Cmd": []string{"mkdir", "/tmp/ci-job-claimed"}}, &exec) != nil || exec.ID == "" {
		return false
	}
	if docker("POST", "/exec/"+exec.ID+"/start", obj{"Detach": false, "Tty": false}, nil) != nil {
		return false
	}
	var result struct {
		Running  bool
		ExitCode int
	}
	if docker("GET", "/exec/"+exec.ID+"/json", nil, &result) != nil {
		return false
	}
	return !result.Running && result.ExitCode == 0
}
func (f *fleet) limits() (time.Duration, time.Duration) {
	idle, life := f.idle, f.lifetime
	if idle == 0 {
		idle = 300 * time.Second
	}
	if life == 0 {
		life = 3600 * time.Second
	}
	return idle, life
}

var validJobName = regexp.MustCompile(`^ci-job-[a-f0-9]{8}-[a-f0-9]{3}$`)

func (f *fleet) recover() error {
	filter, _ := json.Marshal(obj{"label": []string{"ci-runner.owner=" + f.owner}})
	query := "?filters=" + url.QueryEscape(string(filter))
	var containers []struct {
		Names  []string
		Labels map[string]string
	}
	if err := docker("GET", "/containers/json"+query+"&all=true", nil, &containers); err != nil {
		return err
	}
	var networks []struct {
		Name    string
		Created time.Time
		Labels  map[string]string
	}
	if err := docker("GET", "/networks"+query, nil, &networks); err != nil {
		return err
	}
	groups := map[string]*jobState{}
	nets := map[string]string{}
	get := func(n string) *jobState {
		if groups[n] == nil {
			groups[n] = &jobState{recovered: true}
		}
		return groups[n]
	}
	for _, c := range containers {
		if c.Labels["ci-runner.owner"] != f.owner {
			continue
		}
		for _, alias := range c.Names {
			name := strings.TrimPrefix(alias, "/")
			n := jobName(name)
			if !validJobName.MatchString(n) || (name != n && name != n+"-pg" && name != n+"-proxy" && name != n+"-fw") {
				continue
			}
			if label := c.Labels["ci-runner.job"]; label != "" && label != n {
				continue
			}
			j := get(n)
			j.resources = append(j.resources, name)
			if name == n {
				j.gate = c.Labels["ci-runner.idle-gate"] == "1"
			}
		}
	}
	for _, net := range networks {
		n := strings.TrimSuffix(net.Name, "-net")
		if net.Labels["ci-runner.owner"] != f.owner || !validJobName.MatchString(n) || net.Name != n+"-net" {
			continue
		}
		if label := net.Labels["ci-runner.job"]; label != "" && label != n {
			continue
		}
		j := get(n)
		j.created = net.Created
		nets[n] = net.Name
	}
	for n, j := range groups {
		found := false
		// Always delete the runner before any companions, including after partial cleanup.
		for i, r := range j.resources {
			if r == n {
				j.resources[0], j.resources[i] = j.resources[i], j.resources[0]
				found = true
				break
			}
		}
		if !found {
			j.cleaning = true
		} else {
			var v containerState
			err := docker("GET", "/containers/"+n+"/json", nil, &v)
			if errors.Is(err, errMissing) || (err == nil && !v.State.Running) {
				j.cleaning = true
			}
			if err == nil && !v.Created.IsZero() && (j.created.IsZero() || v.Created.Before(j.created)) {
				j.created = v.Created
			}
			// Docker always supplies Created. Do not silently reset a missing deadline.
			if j.created.IsZero() && !j.cleaning {
				return fmt.Errorf("cannot recover creation time for %s", n)
			}
		}
		f.mu.Lock()
		if f.states == nil {
			f.states = map[string]*jobState{}
		}
		if f.jobs == nil {
			f.jobs = map[string]string{}
		}
		f.states[n] = j
		f.jobs[n] = nets[n]
		f.mu.Unlock()
	}
	return nil
}
func (f *fleet) maintain(ctx context.Context, interval time.Duration) {
	for {
		f.reap()
		select {
		case <-f.changes:
			if f.demandSource != nil && f.scaleMu.TryLock() {
				err := f.reconcileCurrent(ctx)
				f.scaleMu.Unlock()
				if err != nil && ctx.Err() == nil {
					f.changed()
				}
			} else if ctx.Err() == nil {
				f.changed()
			}
		default:
		}
		if !pause(ctx, interval) {
			return
		}
	}
}

// Keep the pending message through Scale and ack retries. Do not re-run the SDK
// listener's synthetic initial statistics, which are an old session snapshot.
func (f *fleet) listen(ctx context.Context, delay time.Duration) error {
	// A NEW server-created session has fresh authoritative demand. Apply it once,
	// never on each polling retry. Already acquired jobs otherwise starve after
	// controller downtime when no additional queue message is generated.
	const bootstrapID = -2
	f.scaleMu.Lock()
	delete(f.messages, bootstrapID)
	f.scaleMu.Unlock()
	session := f.session.Session()
	if session.Statistics != nil || f.demandSource != nil {
		initial := &scaleset.RunnerScaleSetMessage{MessageID: bootstrapID, Statistics: session.Statistics}
		if err := retrySession(ctx, delay, func() error { return f.Scale(ctx, initial) }); err != nil {
			return err
		}
		f.scaleMu.Lock()
		delete(f.messages, bootstrapID)
		f.scaleMu.Unlock()
	}
	last := 0
	for ctx.Err() == nil {
		var msg *scaleset.RunnerScaleSetMessage
		if err := retrySession(ctx, delay, func() error { var err error; msg, err = f.session.GetMessage(ctx, last, 3); return err }); err != nil {
			return err
		}
		if msg == nil {
			continue
		}
		if err := retrySession(ctx, delay, func() error { return f.Scale(ctx, msg) }); err != nil {
			return err
		}
		if err := retrySession(ctx, delay, func() error {
			err := f.session.DeleteMessage(ctx, msg.MessageID)
			if errors.Is(err, scaleset.NotFoundError) {
				return nil
			}
			return err
		}); err != nil {
			return err
		}
		last = msg.MessageID
		// The server has confirmed deletion; this message can no longer be
		// redelivered. Keep only pending work/acks in memory.
		f.scaleMu.Lock()
		delete(f.messages, msg.MessageID)
		f.scaleMu.Unlock()
	}
	return ctx.Err()
}
func timeoutConfig() (time.Duration, time.Duration, error) {
	read := func(key string, def int) (time.Duration, error) {
		v := os.Getenv(key)
		if v == "" {
			return time.Duration(def) * time.Second, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 86400 {
			return 0, fmt.Errorf("%s must be 1..86400 seconds", key)
		}
		return time.Duration(n) * time.Second, nil
	}
	idle, err := read("RUNNER_IDLE_TIMEOUT_SECONDS", 300)
	if err != nil {
		return 0, 0, err
	}
	life, err := read("RUNNER_MAX_LIFETIME_SECONDS", 3600)
	if err != nil {
		return 0, 0, err
	}
	if idle > life {
		return 0, 0, fmt.Errorf("idle timeout must not exceed maximum lifetime")
	}
	return idle, life, nil
}
func retry(ctx context.Context, delay time.Duration, fn func() error) error {
	return retryErrors(ctx, delay, fn, false)
}
func retrySession(ctx context.Context, delay time.Duration, fn func() error) error {
	return retryErrors(ctx, delay, fn, true)
}
func retryErrors(ctx context.Context, delay time.Duration, fn func() error, session bool) error {
	for ctx.Err() == nil {
		if err := fn(); err == nil {
			return nil
		} else if session && (errors.Is(err, scaleset.NotFoundError) || errors.Is(err, scaleset.UnauthorizedError)) {
			return err
		}
		if !pause(ctx, delay) {
			break
		}
		delay = min(30*time.Second, delay*2)
	}
	return ctx.Err()
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (f *fleet) drain(interval time.Duration) bool {
	f.mu.Lock()
	f.stopping = true
	f.mu.Unlock()
	_, life := f.limits()
	deadline := time.Now().Add(life)
	for len(f.snapshot()) > 0 {
		if time.Now().After(deadline) {
			return false
		}
		pause(context.Background(), min(interval, time.Until(deadline)))
	}
	return true
}
