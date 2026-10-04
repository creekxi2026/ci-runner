#!/bin/sh
set -eu
n=0
while [ ! -e /tmp/ci-network-ready ]; do
 n=$((n+1)); [ "$n" -lt 120 ] || exit 1
 sleep 1
done
# The read-only image stays immutable; each job initializes its bounded tmpfs.
cp -a /opt/actions-runner/. /home/runner/
export RUNNER_TOOL_CACHE=/home/runner/_work/_tool
/opt/ci/toolcache-init.sh "$RUNNER_TOOL_CACHE"
# Atomic assignment gate shared with the controller's idle cancellation check.
# Job-writable metadata is advisory only; the controller enforces lifetime.
cat > /tmp/ci-job-started.sh <<'HOOK'
#!/bin/sh
set -eu
mkdir /tmp/ci-job-claimed
HOOK
chmod 700 /tmp/ci-job-started.sh
export ACTIONS_RUNNER_HOOK_JOB_STARTED=/tmp/ci-job-started.sh
cd /home/runner
exec ./run.sh
