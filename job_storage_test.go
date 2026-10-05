package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Stateful Docker volume endpoints for tests of complete provisioning flows.
func withJobDiskEngine(t *testing.T, next http.HandlerFunc) {
	volumes := map[string]diskVolume{}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.47")
		if p == "/volumes/create" {
			var v diskVolume
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Fatal(err)
			}
			volumes[v.Name] = v
			w.WriteHeader(201)
		} else if strings.HasPrefix(p, "/volumes/") {
			n := strings.TrimPrefix(p, "/volumes/")
			v, ok := volumes[n]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				delete(volumes, n)
				w.WriteHeader(204)
			} else {
				json.NewEncoder(w).Encode(v)
			}
		} else {
			next(w, r)
		}
	})
}

func TestJobDiskRejectsExistingVolume(t *testing.T) {
	f := &fleet{owner: "unit"}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("mutated an existing disk")
		}
		fmt.Fprint(w, `{"Name":"job-disk","Driver":"local"}`)
	})
	if f.prepareJobDisk(context.Background(), "job", obj{}) == nil {
		t.Fatal("reused existing disk")
	}
	if f.removeJobDisk("job") == nil {
		t.Fatal("removed unowned disk")
	}
}

func TestJobDiskInitializationFailsClosed(t *testing.T) {
	for _, result := range []string{`{"StatusCode":1}`, `{}`} {
		t.Run(result, func(t *testing.T) {
			f := &fleet{owner: "unit", image: "test"}
			withJobDiskEngine(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/wait") {
					fmt.Fprint(w, result)
				} else {
					w.WriteHeader(201)
				}
			})
			host := obj{}
			if f.prepareJobDisk(context.Background(), "job", host) == nil || host["Mounts"] != nil {
				t.Fatal("exposed failed disk")
			}
			if err := f.removeJobDisk("job"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedDiskStartCleansPartialJob(t *testing.T) {
	name := ""
	f := &fleet{owner: "unit", client: &fakeRunners{}, jobs: map[string]string{}}
	withJobDiskEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			name = jobName(r.URL.Query().Get("name"))
			w.WriteHeader(201)
		case strings.HasSuffix(r.URL.Path, "/json"):
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{name + "-net": obj{"IPAddress": "172.20.0.2"}}}})
		case strings.Contains(r.URL.Path, "/wait"):
			fmt.Fprint(w, `{"StatusCode":1}`)
		default:
			w.WriteHeader(204)
		}
	})
	if f.start(context.Background()) == nil || name == "" {
		t.Fatal("failed initializer did not abort provisioning")
	}
	if len(f.jobs) != 0 {
		t.Fatal("partial job was retained after successful cleanup")
	}
	var v diskVolume
	if err := docker("GET", "/volumes/"+jobVolume(name), nil, &v); err != errMissing {
		t.Fatalf("failed start leaked disk: %v", err)
	}
}

func TestJobDiskCleanupRetainsFailedDeletion(t *testing.T) {
	f := &fleet{owner: "unit", jobs: map[string]string{"job": "job-net"}}
	f.state("job").disk = true
	fail := true
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.47")
		if strings.HasPrefix(p, "/volumes/") {
			if p != "/volumes/job-disk" {
				t.Fatal("touched shared cache")
			}
			if r.Method == "GET" {
				json.NewEncoder(w).Encode(obj{"Name": "job-disk", "Driver": "local", "Labels": f.resourceLabels("job")})
				return
			}
			if r.URL.RawQuery != "" {
				t.Fatal("forced volume removal")
			}
			if fail {
				w.WriteHeader(409)
				return
			}
		}
		if fail && p == "/networks/job-net" {
			t.Fatal("lost cleanup tombstone")
		}
		w.WriteHeader(204)
	})
	f.cleanup("job", "job-net")
	if len(f.jobs) != 1 {
		t.Fatal("forgot failed disk cleanup")
	}
	fail = false
	f.reap()
	if len(f.jobs) != 0 {
		t.Fatal("did not retry disk cleanup")
	}
}

func TestRecoveryFindsVolumeOnlyOrphan(t *testing.T) {
	n := "ci-job-12345678-123"
	f := &fleet{owner: "unit"}
	removed := false
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.47")
		v := obj{"Name": jobVolume(n), "Driver": "local", "Labels": f.resourceLabels(n)}
		switch p {
		case "/containers/json", "/networks":
			fmt.Fprint(w, `[]`)
		case "/volumes":
			if !strings.Contains(r.URL.Query().Get("filters"), "ci-runner.owner=unit") {
				t.Fatal("unscoped disk recovery")
			}
			json.NewEncoder(w).Encode(obj{"Volumes": []obj{v, {"Name": "ci-deps-linux-arm64-cache", "Driver": "local", "Labels": f.resourceLabels(n)}}})
		case "/volumes/" + jobVolume(n):
			if r.Method == "DELETE" {
				removed = true
				w.WriteHeader(204)
			} else {
				json.NewEncoder(w).Encode(v)
			}
		default:
			t.Fatalf("unexpected cleanup %s", p)
		}
	})
	if err := f.recover(); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs) != 1 || !f.state(n).cleaning {
		t.Fatal("lost orphan")
	}
	f.reap()
	if !removed || len(f.jobs) != 0 {
		t.Fatal("orphan disk leaked")
	}
}
