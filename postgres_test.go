package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDefaultJobHasNoDatabase(t *testing.T) {
	creates := []string{}
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			name := r.URL.Query().Get("name")
			creates = append(creates, name)
			var v struct{ Env, Cmd []string }
			json.NewDecoder(r.Body).Decode(&v)
			for _, e := range v.Env {
				if strings.HasPrefix(e, "DATABASE_URL=") || strings.HasPrefix(e, "CI_DATABASE_HOST=") {
					t.Error("default job exposes database config")
				}
			}
			if strings.HasSuffix(name, "-fw") && len(v.Cmd) != 2 {
				t.Error("default firewall allows database")
			}
			w.WriteHeader(201)
		case strings.HasSuffix(r.URL.Path, "/json"):
			json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.47/containers/"), "-proxy/json") + "-net": obj{"IPAddress": "172.20.0.2"}}}})
		case strings.Contains(r.URL.Path, "/wait"):
			json.NewEncoder(w).Encode(obj{"StatusCode": 0})
		case strings.HasSuffix(r.URL.Path, "/exec"):
			json.NewEncoder(w).Encode(obj{"Id": "ready"})
		default:
			w.WriteHeader(200)
		}
	})
	f := &fleet{client: &fakeRunners{}, jobs: map[string]string{}}
	if err := f.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range creates {
		if strings.HasSuffix(n, "-pg") {
			t.Fatal("default job created postgres")
		}
	}
}
