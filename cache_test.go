package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCacheMountInitializationAndOwnership(t *testing.T) {
	events, _ := allowedEvents("workflow_dispatch")
	c, _ := dependencyCacheConfig("trusted-manual", "main", "https://github.com/acme/repo", "private", events)
	f := &fleet{cache: c, owner: "private", image: "runner"}
	created, initialized, removed := false, false, false
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1.47/volumes/"):
			if !created {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(obj{"Name": c.volume, "Labels": c.labels(), "Driver": "local", "Options": obj{}})
		case r.URL.Path == "/v1.47/volumes/create":
			var b obj
			json.NewDecoder(r.Body).Decode(&b)
			if b["Name"] != c.volume {
				t.Error("wrong volume")
			}
			created = true
			json.NewEncoder(w).Encode(obj{"Name": c.volume})
		case r.URL.Path == "/v1.47/containers/create":
			var b obj
			json.NewDecoder(r.Body).Decode(&b)
			if b["User"] != "0" {
				t.Error("initializer not root")
			}
			host := b["HostConfig"].(map[string]any)
			if host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true {
				t.Error("unsafe helper")
			}
			caps := host["CapAdd"].([]any)
			if len(caps) != 3 || caps[0] != "CHOWN" || caps[1] != "DAC_OVERRIDE" || caps[2] != "FOWNER" {
				t.Error("initializer must retain FOWNER to chmod directories after ownership changes")
			}
			if !reflect.DeepEqual(b["Cmd"], []any{"python3", "/opt/ci/cache-init.py", "--volume-root", "/opt/ci-cache-volume"}) {
				t.Fatalf("wrong initializer: %v", b["Cmd"])
			}
			initialized = true
		case strings.HasSuffix(r.URL.Path, "/wait"):
			json.NewEncoder(w).Encode(obj{"StatusCode": 0})
		case r.Method == "DELETE":
			if strings.Contains(r.URL.Path, "/volumes/") {
				t.Error("volume removed")
			}
			removed = true
		}
	})
	host := secure()
	env := []string{"HOME=/home/runner"}
	var err error
	env, err = f.prepareDependencyCache(context.Background(), "ci-job-12345678-123", host, env)
	if err != nil {
		t.Fatal(err)
	}
	if !created || !initialized || !removed {
		t.Fatal("incomplete initialization")
	}
	if len(host["Mounts"].([]obj)) != 2 || len(env) != 6 {
		t.Fatalf("mount/env missing: %v %v", host, env)
	}
	mounts := host["Mounts"].([]obj)
	if mounts[0]["Target"] != "/opt/ci-cache" || mounts[0]["VolumeOptions"].(obj)["Subpath"] != "data" || mounts[1]["Target"] != "/opt/ci-tools" || mounts[1]["ReadOnly"] != true || mounts[1]["VolumeOptions"].(obj)["Subpath"] != "data/tools" {
		t.Fatal("worker must mount canonical data and read-only tools")
	}
	f.cache = nil
	host = secure()
	env, err = f.prepareDependencyCache(context.Background(), "job", host, nil)
	if err != nil || len(env) != 0 || host["Mounts"] != nil {
		t.Fatal("cache off not inert")
	}
}

func TestCacheRejectsForeignVolumeAndFailedInitializer(t *testing.T) {
	for _, failure := range []string{"foreign", "repository", "lane", "driver", "options", "exit", "missing-exit"} {
		t.Run(failure, func(t *testing.T) {
			events, _ := allowedEvents("workflow_dispatch")
			c, _ := dependencyCacheConfig("trusted-manual", "main", "https://github.com/acme/repo", "private", events)
			f := &fleet{cache: c, owner: "private", image: "runner"}
			withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/volumes/") {
					labels := c.labels()
					driver := "local"
					options := obj{}
					if failure == "foreign" {
						labels["ci-runner.cache-owner"] = "foreign"
					}
					if failure == "repository" {
						labels["ci-runner.cache-repository"] = "https://github.com/acme/other"
					}
					if failure == "lane" {
						labels["ci-runner.cache-lane"] = "untrusted"
					}
					if failure == "driver" {
						driver = "nfs"
					}
					if failure == "options" {
						options["device"] = "/host"
					}
					json.NewEncoder(w).Encode(obj{"Name": c.volume, "Labels": labels, "Driver": driver, "Options": options})
				} else if strings.HasSuffix(r.URL.Path, "/wait") {
					if failure == "exit" {
						json.NewEncoder(w).Encode(obj{"StatusCode": 1})
					} else {
						json.NewEncoder(w).Encode(obj{})
					}
				}
			})
			host := secure()
			if _, err := f.prepareDependencyCache(context.Background(), "ci-job-12345678-123", host, nil); err == nil {
				t.Fatal("unsafe cache accepted")
			}
			if host["Mounts"] != nil {
				t.Fatal("failed initialization mounted cache")
			}
		})
	}
}

func TestCacheOptInAndTrustBoundary(t *testing.T) {
	events, _ := allowedEvents("workflow_dispatch")
	for _, tc := range []struct {
		mode, lane, repo, owner string
		want                    bool
	}{
		{"", "", "", "", true},
		{"off", "", "", "", true},
		{"trusted-manual", "reviewed-main", "https://github.com/acme/repo", "private", true},
		{"trusted-manual", "", "https://github.com/acme/repo", "private", false},
		{"shared", "main", "https://github.com/acme/repo", "private", false},
		{"trusted-manual", "../main", "https://github.com/acme/repo", "private", false},
	} {
		_, err := dependencyCacheConfig(tc.mode, tc.lane, tc.repo, tc.owner, events)
		if (err == nil) != tc.want {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	for _, event := range []string{"push", "pull_request", "pull_request_target", "schedule"} {
		mixed, _ := allowedEvents("workflow_dispatch," + event)
		if _, err := dependencyCacheConfig("trusted-manual", "reviewed-main", "https://github.com/acme/repo", "private", mixed); err == nil {
			t.Errorf("allowed %s writable sharing", event)
		}
	}
	a, _ := dependencyCacheConfig("trusted-manual", "main", "https://github.com/acme/repo", "private", events)
	for _, tc := range []struct{ lane, repo, owner string }{
		{"candidate", "https://github.com/acme/repo", "private"},
		{"main", "https://github.com/acme/other", "private"},
		{"main", "https://github.com/acme/repo", "public"},
	} {
		b, err := dependencyCacheConfig("trusted-manual", tc.lane, tc.repo, tc.owner, events)
		if err != nil || a.volume != b.volume || reflect.DeepEqual(a.labels(), b.labels()) {
			t.Fatalf("human-readable name must retain distinct ownership labels: %v", err)
		}
	}
	if a.labels()["ci-runner.job"] != nil || a.labels()["ci-runner.owner"] != nil {
		t.Fatal("cache matches janitor resources")
	}
}

func TestCacheExplicitReadableName(t *testing.T) {
	events, _ := allowedEvents("workflow_dispatch")
	for _, name := range []string{"", "ci-deps-linux-arm64-cache", "ci-deps-linux-arm64-cache-secondary"} {
		c, err := dependencyCacheConfig("trusted-manual", "main", "https://github.com/acme/repo", "private", events, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "" {
			name = "ci-deps-linux-arm64-cache"
		}
		if c.volume != name {
			t.Fatal(c.volume)
		}
	}
	for _, name := range []string{"../cache", "/cache", "cache/name", "cache name"} {
		if _, err := dependencyCacheConfig("trusted-manual", "main", "https://github.com/acme/repo", "private", events, name); err == nil {
			t.Fatal("invalid name accepted", name)
		}
	}
}
