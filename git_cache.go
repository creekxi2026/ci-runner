package main

// Persistent Git objects are fetched by this trusted controller, never saved
// from a worker. Normal actions/checkout remains the source/SHA authority.
import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type gitObjectCache struct {
	root, repository, refs, token string
}

func (f *fleet) openGitObjectCache(ctx context.Context, mode, token, repository, refs string) error {
	if mode == "off" {
		return nil
	}
	if mode != "" && mode != "auto" && mode != "on" {
		return fmt.Errorf("GIT_OBJECT_CACHE_MODE must be auto, on or off")
	}
	if f.cache != nil && (mode == "" || mode == "auto") {
		log.Print("Git object cache disabled for legacy shared-writable dependency cache")
		return nil
	}
	repo := strings.TrimPrefix(repository, "https://github.com/")
	if f.work == nil || f.cache != nil || repo == repository || !cacheRepo.MatchString(repo) ||
		strings.HasSuffix(repo, ".git") || strings.Contains(repo, "..") {
		return fmt.Errorf("Git cache requires a fixed GitHub repository and private shared-work jobs")
	}
	if refs == "" {
		refs = "refs/heads/main"
	}
	// Validate the physical pool identity, separately from the repository/owner
	// namespace. Legacy dependency labels describe the volume's initial creator.
	var volume diskVolume
	if err := dockerContext(ctx, "GET", "/volumes/ci-deps-linux-arm64-cache", nil, &volume); err != nil {
		return err
	}
	if volume.Name != "ci-deps-linux-arm64-cache" || volume.Driver != "local" || len(volume.Options) != 0 || len(volume.Labels) != 4 ||
		volume.Labels["ci-runner.cache-schema"] != "v2-linux-arm64-trusted-manual" || volume.Labels["ci-runner.cache-owner"] == "" ||
		volume.Labels["ci-runner.cache-repository"] == "" || volume.Labels["ci-runner.cache-lane"] == "" {
		return fmt.Errorf("Git cache volume identity mismatch")
	}
	var self struct {
		Mounts []struct {
			Type, Name, Destination string
			RW                      bool
		}
	}
	if err := dockerContext(ctx, "GET", "/containers/"+os.Getenv("CONTROLLER_CONTAINER")+"/json", nil, &self); err != nil {
		return err
	}
	mounted := false
	for _, m := range self.Mounts {
		if m.Type == "volume" && m.Name == volume.Name && m.Destination == "/var/lib/ci-runner/cache" && m.RW {
			mounted = true
		}
	}
	if !mounted {
		return fmt.Errorf("Git cache controller mount mismatch")
	}
	base, err := os.OpenRoot("/var/lib/ci-runner/cache")
	if err != nil {
		return err
	}
	defer base.Close()
	st, err := base.Stat(".")
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("Git cache volume root is not protected")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(repo+"\n"+refs)))
	rel := "git-v1/" + f.owner + "/" + key
	for _, dir := range []string{"git-v1", "git-v1/" + f.owner, rel} {
		if err = base.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		st, err = base.Lstat(dir)
		if err != nil {
			return err
		}
		if !privateCacheDir(st) {
			return fmt.Errorf("Git cache namespace is not private root-owned storage")
		}
	}
	f.gitCache = &gitObjectCache{root: filepath.Join(base.Name(), rel), repository: repo, refs: refs, token: token}
	return nil
}

func (f *fleet) prepareGitObjects(ctx context.Context, name string) error {
	if f.gitCache == nil || f.work == nil {
		return nil
	}
	c := f.gitCache
	// Includes lock waiting, network I/O, verification and private copy. Cold
	// timeout simply leaves actions/checkout to fetch with its own credentials.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	home := filepath.Join(f.work.root.Name(), "jobs", name, "home")
	cmd := exec.CommandContext(ctx, "python3", "/opt/ci/git-cache.py", "--root", c.root,
		"--home", home, "--repository", c.repository, "--refs", c.refs)
	cmd.Stdin = strings.NewReader(c.token)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "PYTHONDONTWRITEBYTECODE=1"}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	// The helper handles SIGTERM by killing/reaping the complete Git process
	// group and removing staging; never leave a fetch running after cancellation.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.Output()
	if err != nil {
		// Worker has not started. Remove only our pre-created repository; no
		// job-controlled links or alternate checkout paths have been consumed.
		repo := strings.Split(c.repository, "/")[1]
		if e := f.work.root.RemoveAll("jobs/" + name + "/home/_work/" + repo); e != nil {
			return fmt.Errorf("Git cache private cleanup failed for %s: %w", name, e)
		}
		log.Printf("Git cache unavailable for %s; normal cold checkout", name)
		return nil
	}
	log.Printf("Git cache prepared for %s: %s", name, strings.TrimSpace(string(output)))
	return nil
}
