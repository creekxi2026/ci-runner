package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// All fixtures are invented locally; never use lease credentials in these tests.
func TestPostgresHelperEscapesMasksBeforeEnvironmentPublication(t *testing.T) {
	script, err := os.ReadFile("ci-postgres.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helper, []byte(strings.ReplaceAll(string(script), "/tmp/", dir+"/")), 0700); err != nil {
		t.Fatal(err)
	}
	password := "fixture%25\r\n::warning::not-a-command\n"
	url := "postgres://fixture:" + "fixture%2525@192.0.2.1/ci?x=%0A"
	cfg := "DATABASE_URL='" + url + "'\nCI_DATABASE_HOST='192.0.2.1'\nPGPASSWORD='" + password + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "ci-postgres.env"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	// A directory makes publication fail: both masks must already be on stdout.
	// This verifies ordering through execution, not through source matching.
	cmd := exec.Command("sh", helper)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_ENV=" + dir}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	if stderr.Len() == 0 {
		t.Fatal("fixture must reject publication to a directory")
	}
	escape := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	want := "::add-mask::" + escape.Replace(password) + "\n::add-mask::" + escape.Replace(url) + "\n"
	if stdout.String() != want {
		t.Fatal("both correctly escaped masks must precede any attempted publication")
	}
	if strings.Contains(stderr.String(), password) || strings.Contains(stderr.String(), url) {
		t.Fatal("publication failure exposed fixture secrets")
	}
	cmd = exec.Command("sh", helper, "python3", "-c", `import os; assert os.environ["PGPASSWORD"].startswith("fixture%25"); assert os.environ["DATABASE_URL"].startswith("postgres://fixture:")`)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatal("non-Actions invocation must silently propagate config to its command")
	}
}

func TestPostgresHelperMasksSecretsAndPropagatesEnv(t *testing.T) {
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
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// Observe publication state when the real mask encoder starts. If masks
	// move after the GITHUB_ENV append, this wrapper rejects that invocation.
	wrapper := "#!/bin/sh\n[ ! -e \"$GITHUB_ENV\" ] || exit 73\nexec '" + strings.ReplaceAll(python, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", helper)
	// Keep the fixture independent from an actual CI job's existing database
	// bypass addresses while exercising propagation of both proxy spellings.
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GITHUB_ENV=" + gh, "no_proxy=localhost,127.0.0.1", "NO_PROXY=localhost,127.0.0.1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %s %v", out, err)
	}
	wantMasks := "::add-mask::unit-secret\n::add-mask::" + "postgres://postgres:" + "unit-secret" + "@172.20.0.4:5432/ci?sslmode=disable\n"
	if string(out) != wantMasks {
		t.Fatal("helper must emit only password and complete URL mask commands")
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
