package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPairedLeaseIdentityIsStableAndJobScoped(t *testing.T) {
	l := pairedLease("owner", "job", "closet_ai_test_", "fixture-bootstrap")
	if l != pairedLease("owner", "job", "closet_ai_test_", "fixture-bootstrap") {
		t.Fatal("retry changed lease")
	}
	for _, other := range []databaseLease{
		pairedLease("other-owner", "job", "closet_ai_test_", "fixture-bootstrap"),
		pairedLease("owner", "other-job", "closet_ai_test_", "fixture-bootstrap"),
	} {
		if l.name == other.name || l.user == other.user || l.password == other.password {
			t.Fatal("leases cross identity boundary")
		}
	}
	if !regexp.MustCompile(`^closet_ai_test_[0-9a-f]{24}$`).MatchString(l.name) || l.companion != l.name+"_staging" {
		t.Fatal("incorrect deterministic pair shape")
	}
}
func TestLeaseURLSupportsIPv6(t *testing.T) {
	l := databaseLease{"ci", "", "postgres", "fixture-password"}
	for _, entry := range l.env("2001:db8::1") {
		if strings.HasPrefix(entry, "DATABASE_URL=") && entry != "DATABASE_URL=postgres://postgres:fixture-password@[2001:db8::1]:5432/ci?sslmode=disable" {
			t.Fatal("IPv6 database URL lacks bracketed host")
		}
	}
}
func TestDatabasePrefixValidation(t *testing.T) {
	for _, prefix := range []string{"", "closet_ai_test_", "x_", strings.Repeat("a", 30) + "_"} {
		if err := validateDatabasePrefix(prefix); err != nil {
			t.Fatalf("valid prefix rejected: %v", err)
		}
	}
	for _, prefix := range []string{"a", "_", "Upper_", "x-", "x';_", "1x_", strings.Repeat("a", 31) + "_", " x_", "x_\n"} {
		if validateDatabasePrefix(prefix) == nil {
			t.Fatalf("accepted invalid prefix %q", prefix)
		}
	}
}
func TestPairExportsThroughRealShellWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	l := pairedLease("unit", "job", "closet_ai_test_", strings.Repeat("a", 48))
	script := strings.ReplaceAll(publishLeaseScript, "/tmp/", dir+"/")
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), l.env("172.20.0.4")...)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatal("publication failed or printed output")
	}
	cfg := filepath.Join(dir, "ci-postgres.env")
	info, err := os.Stat(cfg)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe config mode")
	}
	helper, err := os.ReadFile("ci-postgres.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "helper.sh")
	os.WriteFile(path, []byte(strings.ReplaceAll(string(helper), "/tmp/", dir+"/")), 0700)
	gh := filepath.Join(dir, "github-env")
	cmd = exec.Command("sh", path)
	cmd.Env = append(os.Environ(), "GITHUB_ENV="+gh)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatal("helper printed output or failed")
	}
	data, err := os.ReadFile(gh)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.env("172.20.0.4") {
		if !strings.Contains(string(data), e+"\n") {
			t.Fatalf("export missing key %s", strings.SplitN(e, "=", 2)[0])
		}
	}
}
