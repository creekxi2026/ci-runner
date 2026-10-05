package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPerJobDatabaseRequestIsIsolatedAndIdempotent(t *testing.T) {
	for _, scenario := range []string{"default", "requested", "recovered-stopped", "socket-only"} {
		t.Run(scenario, func(t *testing.T) {
			request := scenario != "default"
			recovered := scenario == "recovered-stopped"
			pgStarts := 0
			created := map[string]int{}
			commands := map[string][]string{}
			execs := map[string][]string{}
			next := 0
			publications := 0
			withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.47")
				switch {
				case p == "/containers/create":
					name := r.URL.Query().Get("name")
					created[name]++
					var v struct {
						Cmd        []string
						HostConfig obj
						Env        []string
					}
					json.NewDecoder(r.Body).Decode(&v)
					commands[name] = v.Cmd
					if name == "job-pg" {
						if v.HostConfig["NetworkMode"] != "job-net" || v.HostConfig["PortBindings"] != nil || v.HostConfig["Binds"] != nil {
							t.Error("database escapes job")
						}
					}
					w.WriteHeader(201)
				case strings.HasSuffix(p, "/exec"):
					var v struct{ Cmd []string }
					json.NewDecoder(r.Body).Decode(&v)
					next++
					id := string(rune('a' + next))
					execs[id] = v.Cmd
					if strings.Contains(strings.Join(v.Cmd, " "), "ci-postgres.env") {
						publications++
					}
					json.NewEncoder(w).Encode(obj{"Id": id})
				case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
					id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json")
					code := 0
					if scenario == "socket-only" && strings.HasPrefix(strings.Join(execs[id], " "), "pg_isready ") && strings.Contains(strings.Join(execs[id], " "), "-h 172.20.0.4") {
						code = 2
					}
					if strings.Contains(strings.Join(execs[id], " "), "ci-postgres-request") && !request {
						code = 1
					}
					json.NewEncoder(w).Encode(obj{"Running": false, "ExitCode": code})
				case p == "/containers/job-pg/json":
					if created["job-pg"] == 0 && !recovered {
						w.WriteHeader(404)
					} else {
						json.NewEncoder(w).Encode(obj{"State": obj{"Running": !recovered || pgStarts > 0}, "Config": obj{"Env": []string{"POSTGRES_PASSWORD=" + strings.Repeat("a", 48)}, "Labels": obj{"ci-runner.owner": "unit", "ci-runner.job": "job"}}, "NetworkSettings": obj{"Networks": obj{"job-net": obj{"IPAddress": "172.20.0.4"}}}})
					}
				case p == "/containers/job-pg/start":
					pgStarts++
					w.WriteHeader(204)
				case p == "/containers/job-proxy/json":
					json.NewEncoder(w).Encode(obj{"NetworkSettings": obj{"Networks": obj{"job-net": obj{"IPAddress": "172.20.0.2"}}}})
				case strings.Contains(p, "/wait"):
					json.NewEncoder(w).Encode(obj{"StatusCode": 0})
				default:
					w.WriteHeader(200)
				}
			})
			f := &fleet{owner: "unit", jobs: map[string]string{"job": "job-net"}, lifetime: time.Hour}
			f.state("job").created = time.Now()
			if scenario == "socket-only" {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				f.provisionDatabaseContext(ctx, "job", "job-net")
				if publications != 0 || f.state("job").databaseReady {
					t.Fatal("published config before external TCP readiness")
				}
				return
			}
			f.provisionDatabase("job", "job-net")
			f.provisionDatabase("job", "job-net")
			if request {
				wantCreates := 1
				if recovered {
					wantCreates = 0
					if pgStarts != 1 {
						t.Fatal("recovered stopped database not restarted")
					}
				}
				if created["job-pg"] != wantCreates || publications != 1 {
					t.Fatalf("request creates=%v publications=%d", created, publications)
				}
				if strings.Join(commands["job-fw"], " ") != "/opt/ci/firewall.sh 172.20.0.2 172.20.0.4" {
					t.Fatalf("firewall=%v", commands)
				}
			} else if len(created) != 0 || publications != 0 {
				t.Fatal("unrequested database created")
			}
		})
	}
}
