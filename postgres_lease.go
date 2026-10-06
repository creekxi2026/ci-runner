package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
)

type databaseLease struct{ name, companion, user, password string }

var databasePrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*_$`)

// Named leases require an explicit companion choice; an old prefix-only
// deployment must not silently switch from two databases to one on upgrade.
func databaseCompanionSuffix(prefix, option string) (string, error) {
	if prefix == "" {
		if option != "" && option != "none" {
			return "", fmt.Errorf("POSTGRES_COMPANION_SUFFIX requires POSTGRES_DATABASE_PREFIX")
		}
		return "", nil
	}
	if !databasePrefixPattern.MatchString(prefix) || len(prefix) > 31 {
		return "", fmt.Errorf("invalid POSTGRES_DATABASE_PREFIX: use a lowercase identifier prefix ending in underscore, at most 31 bytes")
	}
	if option == "none" {
		return "", nil
	}
	if !regexp.MustCompile(`^_[a-z0-9_]+$`).MatchString(option) || len(prefix)+24+len(option) > 63 {
		return "", fmt.Errorf("set POSTGRES_COMPANION_SUFFIX explicitly to none or a lowercase suffix starting with underscore; database names must fit 63 bytes")
	}
	return option, nil
}
func namedLease(owner, job, prefix, companionSuffix, bootstrap string) databaseLease {
	id := sha256.Sum256([]byte(owner + "\x00" + job))
	suffix := hex.EncodeToString(id[:12])
	mac := hmac.New(sha256.New, []byte(bootstrap))
	mac.Write([]byte("ci-runner-lease\x00" + owner + "\x00" + job + "\x00" + prefix))
	companion := ""
	if companionSuffix != "" {
		companion = prefix + suffix + companionSuffix
	}
	return databaseLease{prefix + suffix, companion, "ci_" + suffix, hex.EncodeToString(mac.Sum(nil))}
}
func (l databaseLease) env(host string) []string {
	return []string{"CI_DATABASE_NAME=" + l.name, "CI_DATABASE_COMPANION_NAME=" + l.companion, "CI_DATABASE_HOST=" + host, "PGHOST=" + host, "PGPORT=5432", "PGDATABASE=" + l.name, "PGUSER=" + l.user, "PGPASSWORD=" + l.password, "DATABASE_URL=" + fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", l.user, l.password, net.JoinHostPort(host, "5432"), l.name)}
}

// All SQL substitutions are trusted, validated identifiers or derived hex.
// Bootstrap auth uses the cluster-local socket; errors/output are discarded.
// An interrupted CREATE DATABASE leaves a partial lease: future attempts reject
// it rather than silently repairing ownership or provisioning another pair.
const provisionLeaseScript = `# CI_LEASE_PROVISION
PGOPTIONS='-c log_statement=none -c log_min_error_statement=panic -c log_min_messages=panic -c log_duration=off -c log_min_duration_statement=-1 -c log_min_duration_sample=-1' exec psql -X -q -v ON_ERROR_STOP=1 -U postgres -d ci >/dev/null 2>&1 <<SQL
DO \$lease\$
BEGIN
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='$CI_LEASE_USER') THEN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='$CI_LEASE_USER' AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND rolvaliduntil IS NULL)
   OR EXISTS (SELECT FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname='$CI_LEASE_USER'))
   OR (SELECT count(*) FROM pg_database WHERE datdba=(SELECT oid FROM pg_roles WHERE rolname='$CI_LEASE_USER')) <> (CASE WHEN '$CI_LEASE_COMPANION' = '' THEN 1 ELSE 2 END)
   OR (SELECT count(*) FROM pg_database WHERE datname IN ('$CI_LEASE_PRIMARY','$CI_LEASE_COMPANION') AND datdba=(SELECT oid FROM pg_roles WHERE rolname='$CI_LEASE_USER')) <> (CASE WHEN '$CI_LEASE_COMPANION' = '' THEN 1 ELSE 2 END)
  THEN RAISE EXCEPTION 'inconsistent lease'; END IF;
 ELSE
  IF EXISTS (SELECT FROM pg_database WHERE datname IN ('$CI_LEASE_PRIMARY','$CI_LEASE_COMPANION')) THEN RAISE EXCEPTION 'partial lease'; END IF;
  CREATE ROLE $CI_LEASE_USER LOGIN PASSWORD '$CI_LEASE_PASSWORD' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
END \$lease\$;
SELECT 'CREATE DATABASE $CI_LEASE_PRIMARY OWNER $CI_LEASE_USER' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='$CI_LEASE_PRIMARY') \gexec
SELECT 'CREATE DATABASE $CI_LEASE_COMPANION OWNER $CI_LEASE_USER' WHERE '$CI_LEASE_COMPANION' <> '' AND NOT EXISTS (SELECT FROM pg_database WHERE datname='$CI_LEASE_COMPANION') \gexec
DO \$lease\$ BEGIN
 IF (SELECT count(*) FROM pg_database WHERE datname IN ('$CI_LEASE_PRIMARY','$CI_LEASE_COMPANION') AND datdba=(SELECT oid FROM pg_roles WHERE rolname='$CI_LEASE_USER')) <> (CASE WHEN '$CI_LEASE_COMPANION' = '' THEN 1 ELSE 2 END) THEN RAISE EXCEPTION 'inconsistent ownership'; END IF;
END \$lease\$;
SQL
`
const verifyLeaseScript = `# CI_LEASE_VERIFY
for db in "$CI_DATABASE_NAME" "$CI_DATABASE_COMPANION_NAME"; do
 [ -n "$db" ] || continue
 PGDATABASE="$db" psql -X -q -v ON_ERROR_STOP=1 -c 'SELECT 1' >/dev/null 2>&1 || exit 1
done
`
const publishLeaseScript = `umask 077
{
 printf "CI_DATABASE_NAME='%s'\nCI_DATABASE_COMPANION_NAME='%s'\n" "$CI_DATABASE_NAME" "$CI_DATABASE_COMPANION_NAME"
 printf "DATABASE_URL='%s'\nCI_DATABASE_HOST='%s'\n" "$DATABASE_URL" "$CI_DATABASE_HOST"
 printf "PGHOST='%s'\nPGPORT='%s'\nPGDATABASE='%s'\nPGUSER='%s'\nPGPASSWORD='%s'\n" "$PGHOST" "$PGPORT" "$PGDATABASE" "$PGUSER" "$PGPASSWORD"
} > /tmp/ci-postgres.env.new && mv /tmp/ci-postgres.env.new /tmp/ci-postgres.env
`

func provisionLease(ctx context.Context, n string, l databaseLease, host string) bool {
	env := []string{"CI_LEASE_USER=" + l.user, "CI_LEASE_PASSWORD=" + l.password, "CI_LEASE_PRIMARY=" + l.name, "CI_LEASE_COMPANION=" + l.companion}
	code, err := execCodeEnv(ctx, n+"-pg", "999", []string{"sh", "-c", provisionLeaseScript}, env)
	if err != nil || code != 0 {
		return false
	}
	code, err = execCodeEnv(ctx, n+"-pg", "999", []string{"sh", "-c", verifyLeaseScript}, l.env(host))
	return err == nil && code == 0
}
