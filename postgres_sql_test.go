package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual provisioning SQL in a disposable Unix-socket-only cluster.
// No Docker, GitHub, TCP listener, external database or stored credentials.
func TestLeaseSQLWithLocalPostgres(t *testing.T) {
	for _, tool := range []string{"initdb", "pg_ctl", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("local PostgreSQL tools unavailable")
		}
	}
	dir := t.TempDir()
	data, socket := filepath.Join(dir, "data"), filepath.Join(dir, "socket")
	if err := os.Mkdir(socket, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "PGHOST=" + socket, "PGUSER=postgres", "PGDATABASE=ci"}
	run := func(tool string, args ...string) {
		t.Helper()
		cmd := exec.Command(tool, args...)
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			t.Fatalf("fixture command %s failed: %v", tool, err)
		}
	}
	run("initdb", "-D", data, "-U", "postgres", "--auth-local=trust", "--auth-host=reject", "--no-locale")
	run("pg_ctl", "-D", data, "-l", filepath.Join(dir, "server.log"), "-o", "-c listen_addresses='' -c unix_socket_directories='"+socket+"' -c fsync=off", "-w", "-t", "10", "start")
	t.Cleanup(func() { run("pg_ctl", "-D", data, "-m", "immediate", "-w", "-t", "10", "stop") })
	// Admin access is socket-local trust; worker-role verification uses real SCRAM.
	if err := os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte("local all postgres trust\nlocal all all scram-sha-256\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("pg_ctl", "-D", data, "reload")
	run("psql", "-X", "-q", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "CREATE DATABASE ci")
	sql := func(statement string) string {
		t.Helper()
		cmd := exec.Command("psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(statement)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal("fixture SQL failed")
		}
		return strings.TrimSpace(string(out))
	}
	var fixturePasswords []string
	provision := func(l databaseLease) bool {
		fixturePasswords = append(fixturePasswords, l.password)
		cmd := exec.Command("sh", "-c", provisionLeaseScript)
		cmd.Env = append(append([]string{}, env...), "CI_LEASE_USER="+l.user, "CI_LEASE_PASSWORD="+l.password, "CI_LEASE_PRIMARY="+l.name, "CI_LEASE_COMPANION="+l.companion)
		return cmd.Run() == nil
	}
	verify := func(l databaseLease) bool {
		cmd := exec.Command("sh", "-c", verifyLeaseScript)
		cmd.Env = append(append([]string{}, env...), l.env(socket)...)
		return cmd.Run() == nil
	}
	t.Run("fresh-and-idempotent", func(t *testing.T) {
		l := pairedLease("unit", "fresh", "closet_ai_test_", "fixture-bootstrap")
		if !provision(l) || !verify(l) || !provision(l) || !verify(l) {
			t.Fatal("fresh/retry lease failed")
		}
		flags := sql("SELECT rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls FROM pg_roles WHERE rolname='" + l.user + "'")
		if flags != "f|f|f|f|f" {
			t.Fatal("role has elevated privileges")
		}
		if sql("SELECT count(*) FROM pg_database WHERE datname IN ('"+l.name+"','"+l.companion+"') AND datdba=(SELECT oid FROM pg_roles WHERE rolname='"+l.user+"')") != "2" {
			t.Fatal("pair ownership incorrect")
		}
		wrong := l
		wrong.password = "wrong-fixture-password"
		if verify(wrong) {
			t.Fatal("wrong password authenticated")
		}
		for _, flag := range []string{"SUPERUSER", "CREATEDB", "CREATEROLE", "REPLICATION", "BYPASSRLS"} {
			sql("ALTER ROLE " + l.user + " " + flag)
			if provision(l) {
				t.Fatal("elevated role accepted: " + flag)
			}
			sql("ALTER ROLE " + l.user + " NO" + flag)
		}
		sql("CREATE ROLE fixture_parent; GRANT fixture_parent TO " + l.user)
		if provision(l) {
			t.Fatal("role membership accepted")
		}
		sql("REVOKE fixture_parent FROM " + l.user)
		if !provision(l) {
			t.Fatal("consistent lease rejected")
		}
	})
	t.Run("partial-pair", func(t *testing.T) {
		l := pairedLease("unit", "partial", "closet_ai_test_", "fixture-bootstrap")
		if !provision(l) {
			t.Fatal("fixture lease failed")
		}
		sql("DROP DATABASE " + l.companion)
		if provision(l) {
			t.Fatal("partial pair repaired")
		}
	})
	t.Run("foreign-owner", func(t *testing.T) {
		l := pairedLease("unit", "foreign", "closet_ai_test_", "fixture-bootstrap")
		if !provision(l) {
			t.Fatal("fixture lease failed")
		}
		sql("ALTER DATABASE " + l.companion + " OWNER TO postgres")
		if provision(l) {
			t.Fatal("foreign owner accepted")
		}
	})
	t.Run("existing-foreign-database", func(t *testing.T) {
		l := pairedLease("unit", "preexisting", "closet_ai_test_", "fixture-bootstrap")
		sql("CREATE DATABASE " + l.name)
		if provision(l) {
			t.Fatal("foreign preexisting database accepted")
		}
	})
	log, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal("fixture log unavailable")
	}
	for _, password := range fixturePasswords {
		if strings.Contains(string(log), password) {
			t.Fatal("database server logged lease password")
		}
	}
}
