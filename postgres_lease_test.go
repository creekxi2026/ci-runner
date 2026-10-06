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
	l := namedLease("owner", "job", "example_test_", "_scratch", "fixture-bootstrap")
	if l != namedLease("owner", "job", "example_test_", "_scratch", "fixture-bootstrap") {
		t.Fatal("retry changed lease")
	}
	for _, other := range []databaseLease{
		namedLease("other-owner", "job", "example_test_", "_scratch", "fixture-bootstrap"),
		namedLease("owner", "other-job", "example_test_", "_scratch", "fixture-bootstrap"),
	} {
		if l.name == other.name || l.user == other.user || l.password == other.password {
			t.Fatal("leases cross identity boundary")
		}
	}
	if !regexp.MustCompile(`^example_test_[0-9a-f]{24}$`).MatchString(l.name) || l.companion != l.name+"_scratch" {
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
func TestDatabaseConfigurationValidation(t *testing.T) {
	for _, cfg := range [][2]string{{"", ""}, {"", "none"}, {"example_test_", "none"}, {"x_", "_scratch"}, {strings.Repeat("a", 30) + "_", "_archive"}} {
		if _, err := databaseCompanionSuffix(cfg[0], cfg[1]); err != nil {
			t.Fatalf("valid configuration rejected: %v", err)
		}
	}
	for _, prefix := range []string{"a", "_", "Upper_", "x-", "x';_", "1x_", strings.Repeat("a", 31) + "_", " x_", "x_\n"} {
		if _, err := databaseCompanionSuffix(prefix, "none"); err == nil {
			t.Fatalf("accepted prefix %q", prefix)
		}
	}
	for _, cfg := range [][2]string{{"", "_scratch"}, {"x_", ""}, {"x_", "_"}, {"x_", "Scratch"}, {"x_", "_x';"}, {"x_", "../x"}, {"x_", "_x\n"}, {"x_", "_" + strings.Repeat("a", 37)}} {
		if _, err := databaseCompanionSuffix(cfg[0], cfg[1]); err == nil {
			t.Fatalf("accepted suffix configuration %q", cfg)
		}
	}
	suffix, err := databaseCompanionSuffix("example_test_", "none")
	if err != nil || suffix != "" {
		t.Fatal("none did not disable companion")
	}
	lease := namedLease("owner", "single", "example_test_", suffix, "fixture-bootstrap")
	if lease.name == "" || lease.companion != "" || lease.user == "postgres" {
		t.Fatal("single database lease is not isolated")
	}
}
func TestPairExportsThroughRealShellWithActionsMasks(t *testing.T) {
	dir := t.TempDir()
	l := namedLease("unit", "job", "example_test_", "_scratch", strings.Repeat("a", 48))
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
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_ENV=" + gh}
	want := "::add-mask::" + l.password + "\n"
	for _, entry := range l.env("172.20.0.4") {
		if strings.HasPrefix(entry, "DATABASE_URL=") {
			want += "::add-mask::" + strings.TrimPrefix(entry, "DATABASE_URL=") + "\n"
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != want {
		t.Fatal("helper must emit exactly the paired lease Actions masks")
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
