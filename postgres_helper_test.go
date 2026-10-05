package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresHelperDoesNotPrintSecretsAndPropagatesEnv(t *testing.T) {
	script, err := os.ReadFile("ci-postgres.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	script = []byte(strings.ReplaceAll(string(script), "/tmp/", dir+"/"))
	if err = os.WriteFile(helper, script, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := "DATABASE_URL='postgres://postgres:unit-secret@172.20.0.4:5432/ci?sslmode=disable'\nCI_DATABASE_HOST='172.20.0.4'\nPGPASSWORD='unit-secret'\n"
	if err = os.WriteFile(filepath.Join(dir, "ci-postgres.env"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(dir, "github-env")
	cmd := exec.Command("sh", helper)
	cmd.Env = append(os.Environ(), "GITHUB_ENV="+gh)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %s %v", out, err)
	}
	if len(out) != 0 {
		t.Fatalf("helper printed config: %q", out)
	}
	if _, err = os.Stat(filepath.Join(dir, "ci-postgres-request")); err != nil {
		t.Fatal("request missing")
	}
	env, err := os.ReadFile(gh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "DATABASE_URL=postgres://postgres:unit-secret@") || !strings.Contains(string(env), "NO_PROXY=localhost,127.0.0.1,172.20.0.4") {
		t.Fatal("workflow environment missing config")
	}
}
