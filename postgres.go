package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// execCode never infers completion from a lost start response.
func execCode(ctx context.Context, n, user string, cmd []string) (int, error) {
	return execCodeEnv(ctx, n, user, cmd, nil)
}
func execCodeEnv(ctx context.Context, n, user string, cmd, env []string) (int, error) {
	var ex struct{ ID string }
	if err := dockerContext(ctx, "POST", "/containers/"+n+"/exec", obj{"User": user, "Cmd": cmd, "Env": env}, &ex); err != nil {
		return -1, err
	}
	if ex.ID == "" {
		return -1, fmt.Errorf("missing exec ID")
	}
	if err := dockerContext(ctx, "POST", "/exec/"+ex.ID+"/start", obj{"Detach": false, "Tty": false}, nil); err != nil {
		return -1, err
	}
	for {
		var v struct {
			Running  bool
			ExitCode *int
		}
		if err := dockerContext(ctx, "GET", "/exec/"+ex.ID+"/json", nil, &v); err != nil {
			return -1, err
		}
		if !v.Running && v.ExitCode != nil {
			return *v.ExitCode, nil
		}
		if !pause(ctx, 50*time.Millisecond) {
			return -1, ctx.Err()
		}
	}
}

// Workers are per-job and bounded by BOTH readiness and immutable job lifetime.
// No readiness wait executes on the local janitor or scheduler goroutine.
func (f *fleet) provisionDatabase(n, network string) {
	f.provisionDatabaseContext(context.Background(), n, network)
}
func (f *fleet) provisionDatabaseContext(parent context.Context, n, network string) {
	j := f.state(n)
	if j == nil || !j.mu.TryLock() {
		return
	}
	defer j.mu.Unlock()
	if j.cleaning || j.provisioning || j.databaseReady || j.created.IsZero() {
		return
	}
	_, life := f.limits()
	deadline := minTime(time.Now().Add(45*time.Second), j.created.Add(life))
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	code, err := execCode(ctx, n, "1001", []string{"test", "-f", "/tmp/ci-postgres-request"})
	if err != nil || code != 0 {
		return
	}
	var pg struct {
		State  struct{ Running bool }
		Config struct {
			Labels map[string]string
			Env    []string
		}
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	err = dockerContext(ctx, "GET", "/containers/"+n+"-pg/json", nil, &pg)
	password := ""
	if errors.Is(err, errMissing) {
		var random [24]byte
		if _, err = rand.Read(random[:]); err != nil {
			return
		}
		password = hex.EncodeToString(random[:])
		h := secure()
		if j.disk {
			h["Mounts"] = []obj{f.diskMount(n, "postgres", "/var/lib/postgresql/data"), f.diskMount(n, "postgres-run", "/var/run/postgresql")}
		} else {
			// Preserve already-running legacy jobs during a controller upgrade.
			h["Tmpfs"] = obj{"/var/lib/postgresql/data": "rw,size=512m", "/var/run/postgresql": "rw,size=16m"}
		}
		h["ReadonlyRootfs"] = true
		h["Memory"] = 512 * 1024 * 1024
		h["NanoCpus"] = int64(500000000)
		// Recovery cleanup must know about a companion even after partial creation.
		if j.recovered {
			j.resources = appendUnique(j.resources, n+"-pg", n+"-fw")
		}
		pgEnv := []string{"POSTGRES_PASSWORD=" + password, "POSTGRES_DB=ci", "PGDATA=/var/lib/postgresql/data/pgdata"}
		if f.databasePrefix != "" {
			pgEnv = append(pgEnv, "CI_DATABASE_PREFIX="+f.databasePrefix, "CI_DATABASE_COMPANION_SUFFIX="+f.companionSuffix)
		}
		if f.createContext(ctx, n+"-pg", f.postgresImage(), "999", []string{"postgres"}, pgEnv, h, network) != nil {
			return
		}
	} else if err != nil {
		return
	} else {
		if pg.Config.Labels["ci-runner.owner"] != f.owner || pg.Config.Labels["ci-runner.job"] != n {
			return
		}
		for _, e := range pg.Config.Env {
			if strings.HasPrefix(e, "POSTGRES_PASSWORD=") {
				password = strings.TrimPrefix(e, "POSTGRES_PASSWORD=")
			}
		}
		storedPrefix, storedSuffix := "", ""
		hasSuffix := false
		for _, e := range pg.Config.Env {
			if strings.HasPrefix(e, "CI_DATABASE_PREFIX=") {
				storedPrefix = strings.TrimPrefix(e, "CI_DATABASE_PREFIX=")
			}
			if strings.HasPrefix(e, "CI_DATABASE_COMPANION_SUFFIX=") {
				storedSuffix = strings.TrimPrefix(e, "CI_DATABASE_COMPANION_SUFFIX=")
				hasSuffix = true
			}
		}
		if storedPrefix != f.databasePrefix || storedSuffix != f.companionSuffix || (storedPrefix != "" && !hasSuffix) {
			return
		}
		if len(password) != 48 {
			return
		}
		if _, err := hex.DecodeString(password); err != nil {
			return
		}
		if !pg.State.Running && dockerContext(ctx, "POST", "/containers/"+n+"-pg/start", nil, nil) != nil {
			return
		}
	}
	database, err := f.address(ctx, n+"-pg", network)
	if err != nil || net.ParseIP(database) == nil {
		return
	}
	proxy, err := f.address(ctx, n+"-proxy", network)
	if err != nil || net.ParseIP(proxy) == nil {
		return
	}
	for {
		code, err = execCode(ctx, n+"-pg", "999", []string{"pg_isready", "-h", database, "-p", "5432", "-U", "postgres", "-d", "ci"})
		if err == nil && code == 0 {
			break
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
	var lease databaseLease
	if f.databasePrefix != "" {
		lease = namedLease(f.owner, n, f.databasePrefix, f.companionSuffix, password)
		if !provisionLease(ctx, n, lease, database) {
			return
		}
	}
	// Reuse the fixed owned helper name; remove remnants of a interrupted setup.
	if dockerContext(ctx, "DELETE", "/containers/"+n+"-fw?force=true", nil, nil) != nil {
		return
	}
	h := secure()
	h["CapAdd"] = []string{"NET_ADMIN"}
	h["ReadonlyRootfs"] = true
	if f.createContext(ctx, n+"-fw", f.image, "0", []string{"/opt/ci/firewall.sh", proxy, database}, nil, h, "container:"+n) != nil {
		return
	}
	var wait struct{ StatusCode int }
	if dockerContext(ctx, "POST", "/containers/"+n+"-fw/wait?condition=not-running", nil, &wait) != nil || wait.StatusCode != 0 {
		return
	}
	if dockerContext(ctx, "DELETE", "/containers/"+n+"-fw?force=true", nil, nil) != nil {
		return
	}
	if f.databasePrefix == "" {
		lease = databaseLease{"ci", "", "postgres", password}
	}
	code, err = execCodeEnv(ctx, n, "1001", []string{"sh", "-c", publishLeaseScript}, lease.env(database))
	if err == nil && code == 0 {
		j.databaseReady = true
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func appendUnique(values []string, more ...string) []string {
	for _, v := range more {
		found := false
		for _, old := range values {
			if old == v {
				found = true
			}
		}
		if !found {
			values = append(values, v)
		}
	}
	return values
}
