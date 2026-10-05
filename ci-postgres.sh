#!/bin/sh
# Request only this job's disposable database; never print connection secrets.
set -eu
umask 077
: > /tmp/ci-postgres-request
n=0
while [ ! -f /tmp/ci-postgres.env ]; do
 n=$((n+1))
 if [ "$n" -ge 60 ]; then
  printf '%s\n' 'ci-postgres: database request timed out' >&2
  exit 1
 fi
 sleep 1
done
set -a
. /tmp/ci-postgres.env
no_proxy="${no_proxy:-localhost,127.0.0.1},$CI_DATABASE_HOST"
NO_PROXY="${NO_PROXY:-localhost,127.0.0.1},$CI_DATABASE_HOST"
export no_proxy NO_PROXY
set +a
if [ -n "${GITHUB_ENV:-}" ]; then
 # Register masks before even opening GITHUB_ENV. Python is image-managed;
 # read exported values, never pass credentials in process arguments.
 python3 -c '
import os
for name in ("PGPASSWORD", "DATABASE_URL"):
    value = os.environ[name].replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    print("::add-mask::" + value, flush=True)
 '
 {
  printf 'DATABASE_URL=%s\n' "$DATABASE_URL"
  printf 'CI_DATABASE_HOST=%s\n' "$CI_DATABASE_HOST"
  printf 'CI_DATABASE_NAME=%s\n' "${CI_DATABASE_NAME:-${PGDATABASE:-ci}}"
  printf 'CI_DATABASE_COMPANION_NAME=%s\n' "${CI_DATABASE_COMPANION_NAME:-}"
  printf 'PGHOST=%s\n' "${PGHOST:-$CI_DATABASE_HOST}"
  printf 'PGPORT=%s\n' "${PGPORT:-5432}"
  printf 'PGDATABASE=%s\n' "${PGDATABASE:-ci}"
  printf 'PGUSER=%s\n' "${PGUSER:-postgres}"
  printf 'PGPASSWORD=%s\n' "$PGPASSWORD"
  printf 'no_proxy=%s\nNO_PROXY=%s\n' "$no_proxy" "$NO_PROXY"
 } >> "$GITHUB_ENV"
fi
# Optional command gets the environment in the current step, without eval/output.
if [ "$#" -gt 0 ]; then exec "$@"; fi
