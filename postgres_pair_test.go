package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPairedLeaseDoesNotExposeBootstrapSecrets(t *testing.T) {
	for _, scenario := range []string{"fresh", "recovered", "provision-failed", "verify-failed", "unrequested", "wrong-owner", "prefix-changed", "prefix-disabled", "suffix-changed", "legacy-missing-suffix", "single-fresh", "single-recovered"} {
		t.Run(scenario, func(t *testing.T) {
			created, published, provisions := 0, 0, 0
			bootstrap := strings.Repeat("a", 48)
			var leaseEnv []string
			commands := map[string][]string{}
			count := 0
			withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.47")
				switch {
				case p == "/containers/job-pg/json":
					if (scenario == "fresh" || scenario == "single-fresh") && created == 0 {
						w.WriteHeader(404)
						return
					}
					owner := "unit"
					if scenario == "wrong-owner" {
						owner = "other"
					}
					pgEnv := []string{"POSTGRES_PASSWORD=" + bootstrap, "CI_DATABASE_PREFIX=example_test_"}
					suffix := "_scratch"
					if strings.HasPrefix(scenario, "single-") {
						suffix = ""
					}
					if scenario != "legacy-missing-suffix" {
						pgEnv = append(pgEnv, "CI_DATABASE_COMPANION_SUFFIX="+suffix)
					}
					json.NewEncoder(w).Encode(obj{"State": obj{"Running": true}, "Config": obj{"Labels": obj{"ci-runner.owner": owner, "ci-runner.job": "job"}, "Env": pgEnv}, "NetworkSettings": obj{"Networks": obj{"job-net": obj{"IPAddress": "172.20.0.4"}}}})
				case p == "/containers/job-proxy/json":
					json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{"job-net": obj{"IPAddress": "172.20.0.2"}}}})
				case p == "/containers/create":
					var v struct{ Env []string }
					json.NewDecoder(r.Body).Decode(&v)
					if r.URL.Query().Get("name") == "job-pg" {
						created++
						for _, e := range v.Env {
							if strings.HasPrefix(e, "POSTGRES_PASSWORD=") {
								bootstrap = strings.TrimPrefix(e, "POSTGRES_PASSWORD=")
							}
						}
					}
					w.WriteHeader(201)
				case strings.HasSuffix(p, "/exec"):
					var v struct{ Cmd, Env []string }
					json.NewDecoder(r.Body).Decode(&v)
					count++
					id := string(rune('a' + count))
					commands[id] = v.Cmd
					joined := strings.Join(v.Cmd, " ")
					if strings.Contains(joined, bootstrap) {
						t.Error("bootstrap password in argv")
					}
					if p == "/containers/job/exec" {
						for _, e := range v.Env {
							if strings.Contains(e, bootstrap) {
								t.Error("worker received bootstrap")
							}
						}
						if strings.Contains(joined, "ci-postgres.env") {
							published++
							leaseEnv = v.Env
						}
					}
					if strings.Contains(joined, "CI_LEASE_PROVISION") {
						provisions++
					}
					json.NewEncoder(w).Encode(obj{"Id": id})
				case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
					id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json")
					cmd := strings.Join(commands[id], " ")
					code := 0
					if scenario == "unrequested" && strings.Contains(cmd, "ci-postgres-request") {
						code = 1
					}
					if scenario == "provision-failed" && strings.Contains(cmd, "CI_LEASE_PROVISION") {
						code = 1
					}
					if scenario == "verify-failed" && strings.Contains(cmd, "CI_LEASE_VERIFY") {
						code = 1
					}
					json.NewEncoder(w).Encode(obj{"Running": false, "ExitCode": code})
				case strings.Contains(p, "/wait"):
					json.NewEncoder(w).Encode(obj{"StatusCode": 0})
				default:
					w.WriteHeader(200)
				}
			})
			f := &fleet{owner: "unit", databasePrefix: "example_test_", companionSuffix: "_scratch", jobs: map[string]string{"job": "job-net"}, lifetime: time.Hour}
			if scenario == "prefix-changed" {
				f.databasePrefix = "different_"
			}
			if scenario == "prefix-disabled" {
				f.databasePrefix = ""
			}
			if scenario == "suffix-changed" {
				f.companionSuffix = "_other"
			}
			if strings.HasPrefix(scenario, "single-") {
				f.companionSuffix = ""
			}
			f.state("job").created = time.Now()
			f.provisionDatabase("job", "job-net")
			f.provisionDatabase("job", "job-net")
			good := scenario == "fresh" || scenario == "recovered" || strings.HasPrefix(scenario, "single-")
			if !good {
				if published != 0 || f.state("job").databaseReady {
					t.Fatal("failed lease published")
				}
				return
			}
			if published != 1 || provisions != 1 || !f.state("job").databaseReady {
				t.Fatalf("published=%d provision=%d", published, provisions)
			}
			values := map[string]string{}
			for _, e := range leaseEnv {
				k, v, _ := strings.Cut(e, "=")
				values[k] = v
			}
			name := values["CI_DATABASE_NAME"]
			companion := name + "_scratch"
			if f.companionSuffix == "" {
				companion = ""
			}
			if len(name) != len("example_test_")+24 || !strings.HasPrefix(name, "example_test_") || values["CI_DATABASE_COMPANION_NAME"] != companion || values["PGDATABASE"] != name || values["PGUSER"] == "postgres" || values["PGPASSWORD"] == "" || values["PGPASSWORD"] == bootstrap {
				t.Fatalf("invalid lease metadata (keys=%d)", len(values))
			}
			// Simulate a controller restart against the same private cluster bootstrap state.
			f.state("job").databaseReady = false
			prior := values["PGPASSWORD"]
			f.provisionDatabase("job", "job-net")
			for _, e := range leaseEnv {
				if strings.HasPrefix(e, "PGPASSWORD=") && e != "PGPASSWORD="+prior {
					t.Error("recovery rotated password")
				}
			}
			if created > 1 {
				t.Error("second cluster created")
			}
		})
	}
}
