package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWorkspaceInitializerBudgetAndFailure(t *testing.T) {
	for _, status := range []int{0, 137} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			created := false
			withEngine(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1.47/images/"):
					fmt.Fprintf(w, `{"ID":"sha256:%s"}`, strings.Repeat("a", 64))
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var payload struct{ HostConfig map[string]any }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					h := payload.HostConfig
					if h["Memory"] != float64(1024*1024*1024) || h["MemorySwap"] != h["Memory"] || h["NanoCpus"] != float64(2e9) || h["ReadonlyRootfs"] != true || h["NetworkMode"] != "none" {
						t.Fatalf("initializer resource/isolation contract: %+v", h)
					}
					created = true
					w.WriteHeader(201)
				case strings.Contains(r.URL.Path, "/wait"):
					fmt.Fprintf(w, `{"StatusCode":%d}`, status)
				default:
					w.WriteHeader(204)
				}
			})
			f := &fleet{image: "fixture", work: &sharedWork{volume: "fixture"}}
			err := f.prepareWorkspace(context.Background(), "ci-job-ffffffff-111")
			if !created || (err != nil) != (status != 0) {
				t.Fatalf("created=%v error=%v", created, err)
			}
		})
	}
}
