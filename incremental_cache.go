package main

// Incremental caches are operator-selected data, never a writable worker mount.
// A cache hit does not constitute verification and never changes job admission.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

type incrementalPolicy struct {
	Version      int               `json:"version"`
	Repository   string            `json:"repository"`
	Branch       string            `json:"branch"`
	Workflow     string            `json:"workflow"`
	Job          string            `json:"job"`
	Paths        []string          `json:"paths"`
	MaxBytes     int64             `json:"max_bytes"`
	MaxEntries   int               `json:"max_entries"`
	VolumeLabels map[string]string `json:"volume_labels"`
}
type incrementalCache struct {
	ownerUID, jobUID     int
	mu                   sync.Mutex
	policy               incrementalPolicy
	key, token, endpoint string
	http                 *http.Client
	root                 *os.Root
}

var cacheRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var cacheSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

func parseIncrementalPolicy(b []byte) (incrementalPolicy, error) {
	var p incrementalPolicy
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, fmt.Errorf("trailing cache policy")
	}
	if p.Version != 1 || !cacheRepo.MatchString(p.Repository) || p.Branch == "" || strings.ContainsAny(p.Branch, "\r\n?#@") || p.Job == "" || len(p.Job) > 200 ||
		!strings.HasPrefix(p.Workflow, ".github/workflows/") || path.Clean(p.Workflow) != p.Workflow || strings.ContainsAny(p.Workflow, "\r\n?#@") ||
		p.MaxBytes < 1 || p.MaxBytes > 1<<30 || p.MaxEntries < 1 || p.MaxEntries > 50000 || len(p.Paths) < 1 || len(p.Paths) > 8 {
		return p, fmt.Errorf("invalid incremental cache policy")
	}
	for i, s := range p.Paths {
		if !strings.HasPrefix(s, ".cache/") || path.Clean(s) != s || strings.ContainsAny(s, "\\\x00\r\n") || len(s) > 160 {
			return p, fmt.Errorf("invalid cache directory")
		}
		for _, other := range p.Paths[:i] {
			if s == other || strings.HasPrefix(s, other+"/") || strings.HasPrefix(other, s+"/") {
				return p, fmt.Errorf("overlapping cache directories")
			}
		}
	}
	if len(p.VolumeLabels) != 4 || p.VolumeLabels["ci-runner.cache-schema"] != "v2-linux-arm64-trusted-manual" || p.VolumeLabels["ci-runner.cache-owner"] == "" || p.VolumeLabels["ci-runner.cache-repository"] == "" || p.VolumeLabels["ci-runner.cache-lane"] == "" {
		return p, fmt.Errorf("explicit existing cache volume identity required")
	}
	return p, nil
}

func (f *fleet) openIncrementalCache(ctx context.Context, filename, token, repository string) error {
	if filename == "" {
		return nil
	}
	if f.cache != nil || f.work == nil || f.seed == "" {
		return fmt.Errorf("incremental cache requires private seeded workspaces and shared writes off")
	}
	b, e := os.ReadFile(filename)
	if e != nil {
		return e
	}
	p, e := parseIncrementalPolicy(b)
	if e != nil {
		return e
	}
	if repository != "https://github.com/"+p.Repository {
		return fmt.Errorf("cache repository differs from fleet")
	}
	var v diskVolume
	if e = dockerContext(ctx, "GET", "/volumes/ci-deps-linux-arm64-cache", nil, &v); e != nil {
		return e
	}
	if v.Name != "ci-deps-linux-arm64-cache" || v.Driver != "local" || len(v.Options) != 0 || len(v.Labels) != len(p.VolumeLabels) {
		return fmt.Errorf("incremental cache volume mismatch")
	}
	for k, w := range p.VolumeLabels {
		if v.Labels[k] != w {
			return fmt.Errorf("incremental cache volume labels mismatch")
		}
	}
	// Mount identity must agree with the Docker volume we just inspected.
	var self struct {
		Mounts []struct {
			Type, Name, Destination string
			RW                      bool
		}
	}
	if e = dockerContext(ctx, "GET", "/containers/"+os.Getenv("CONTROLLER_CONTAINER")+"/json", nil, &self); e != nil {
		return e
	}
	mounted := false
	for _, m := range self.Mounts {
		if m.Type == "volume" && m.Name == v.Name && m.Destination == "/var/lib/ci-runner/cache" && m.RW {
			mounted = true
		}
	}
	if !mounted {
		return fmt.Errorf("incremental cache mount mismatch")
	}
	base, e := os.OpenRoot("/var/lib/ci-runner/cache")
	if e != nil {
		return e
	}
	defer base.Close()
	st, e := base.Stat(".")
	if e != nil {
		return e
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || sys.Uid != 0 || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("cache volume root is not protected")
	}
	for _, dir := range []string{"incremental-v1", "incremental-v1/" + f.owner} {
		if e = base.Mkdir(dir, 0700); e != nil && !os.IsExist(e) {
			return e
		}
		st, err := base.Lstat(dir)
		if err != nil {
			return err
		}
		if !privateCacheDir(st) {
			return fmt.Errorf("cache namespace is not private root-owned storage")
		}
	}
	root, e := base.OpenRoot("incremental-v1/" + f.owner)
	if e != nil {
		return e
	}
	canonical, _ := json.Marshal(p)
	sum := sha256.Sum256([]byte(string(canonical) + "\n" + f.image + "\n" + f.seed + "\n" + f.toolsSeed))
	f.incremental = &incrementalCache{jobUID: 1001, policy: p, key: hex.EncodeToString(sum[:]), token: token, endpoint: "https://api.github.com", http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, root: root}
	return nil
}

func (c *incrementalCache) get(ctx context.Context, suffix string, out any) error {
	req, e := http.NewRequestWithContext(ctx, "GET", c.endpoint+"/repos/"+c.policy.Repository+suffix, nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	r, e := c.http.Do(req)
	if e != nil {
		return fmt.Errorf("cache provenance request failed")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("cache provenance HTTP %d", r.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}

type cacheRun struct {
	ID         int64  `json:"id"`
	Attempt    int    `json:"run_attempt"`
	Event      string `json:"event"`
	Path       string `json:"path"`
	SHA        string `json:"head_sha"`
	Branch     string `json:"head_branch"`
	Repository struct {
		Name string `json:"full_name"`
	} `json:"repository"`
	HeadRepository struct {
		Name string `json:"full_name"`
	} `json:"head_repository"`
}
type cacheJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Attempt    int    `json:"run_attempt"`
	RunnerID   int    `json:"runner_id"`
	Runner     string `json:"runner_name"`
	Name       string `json:"name"`
	SHA        string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Server messages select a run, but never attest the executed source by themselves.
func (c *incrementalCache) attest(ctx context.Context, event *scaleset.JobStarted) (string, error) {
	p := c.policy
	if event == nil || event.RunnerID <= 0 || !validJobName.MatchString(event.RunnerName) || event.WorkflowRunID <= 0 || event.EventName != "push" || event.OwnerName+"/"+event.RepositoryName != p.Repository || event.JobDisplayName != p.Job || event.JobWorkflowRef != p.Repository+"/"+p.Workflow+"@refs/heads/"+p.Branch {
		return "", fmt.Errorf("not a configured cache producer")
	}
	var run cacheRun
	if e := c.get(ctx, fmt.Sprintf("/actions/runs/%d", event.WorkflowRunID), &run); e != nil {
		return "", e
	}
	if run.ID != event.WorkflowRunID || run.Attempt < 1 || run.Event != "push" || run.Path != p.Workflow || run.Branch != p.Branch || !cacheSHA.MatchString(run.SHA) || run.Repository.Name != p.Repository || run.HeadRepository.Name != p.Repository {
		return "", fmt.Errorf("cache run identity mismatch")
	}
	var jobs struct {
		Total int        `json:"total_count"`
		Jobs  []cacheJob `json:"jobs"`
	}
	if e := c.get(ctx, fmt.Sprintf("/actions/runs/%d/attempts/%d/jobs?per_page=100", run.ID, run.Attempt), &jobs); e != nil {
		return "", e
	}
	if jobs.Total != len(jobs.Jobs) || jobs.Total > 100 {
		return "", fmt.Errorf("incomplete cache job evidence")
	}
	matches := 0
	for _, job := range jobs.Jobs {
		if job.RunnerID == event.RunnerID && job.Runner == event.RunnerName {
			if job.ID <= 0 || job.RunID != run.ID || job.Attempt != run.Attempt || job.Name != p.Job || job.SHA != run.SHA || job.Status != "completed" || job.Conclusion != "success" {
				return "", fmt.Errorf("cache producer did not succeed with exact identity")
			}
			matches++
		}
	}
	if matches != 1 {
		return "", fmt.Errorf("cache producer not uniquely identified")
	}
	var branch struct {
		Protected bool `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if e := c.get(ctx, "/branches/"+url.PathEscape(p.Branch), &branch); e != nil {
		return "", e
	}
	if !branch.Protected || branch.Commit.SHA != run.SHA {
		return "", fmt.Errorf("cache source is not current protected branch head")
	}
	return run.SHA, nil
}

func (f *fleet) cacheJobEvent(event *scaleset.JobStarted) {
	if f.incremental == nil || event == nil {
		return
	}
	if j := f.state(event.RunnerName); j != nil {
		j.mu.Lock()
		defer j.mu.Unlock()
		copy := *event
		j.cacheEvent = &copy
	}
}
func (f *fleet) saveIncremental(n string, j *jobState) {
	if f.incremental == nil || j.cacheAttempted || j.cacheEvent == nil || !j.shared {
		return
	}
	j.cacheAttempted = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sha, e := f.incremental.attest(ctx, j.cacheEvent)
	if e == nil {
		var home *os.Root
		home, e = f.work.root.OpenRoot("jobs/" + n + "/home")
		if e == nil {
			defer home.Close()
			e = f.incremental.save(ctx, home, sha)
		}
	}
	if e != nil {
		log.Printf("incremental cache: no publication for %s: %v", n, e)
	} else {
		log.Printf("incremental cache: published successful protected source %s from %s", sha, n)
	}
}
func (f *fleet) restoreIncremental(ctx context.Context, n string) {
	if f.incremental == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	home, e := f.work.root.OpenRoot("jobs/" + n + "/home")
	if e == nil {
		defer home.Close()
		e = f.incremental.restore(ctx, home)
	}
	if e != nil {
		log.Printf("incremental cache: unavailable for %s (native cold/seed fallback): %v", n, e)
	}
}
