#!/bin/sh
set -eu
# Material enters only through the controller-owned Docker stdin stream. The
# transient private broker passes a socketpair capability through real Listener.
if [ "${CI_JOB_INTAKE:-}" = 1 ] && [ -z "${CI_INTAKE_BOOTSTRAP_FD:-}" ]; then
 exec /usr/bin/python3 /opt/ci/worker_intake.py bootstrap
fi
if [ "${CI_INTAKE_DIAGNOSTICS:-}" = 1 ]; then
 /usr/bin/python3 -I -c 'import os,select
try:
 if select.select([], [2], [], 0)[1]: os.write(2,b"ci-intake/v1 runner_entry\n")
except (OSError,ValueError): pass' || :
fi
n=0
while [ ! -e /tmp/ci-network-ready ]; do
 n=$((n+1)); [ "$n" -lt 120 ] || exit 1
 sleep 1
done
# The read-only image stays immutable; each job has a private disposable disk.
if [ ! -f /home/runner/.ci-workspace-ready ]; then
 cp -a /opt/actions-runner/. /home/runner/
fi
# Optional immutable tools are also available to mixed-event jobs.
if [ -d /opt/ci-tools/python/lib ]; then
 export LD_LIBRARY_PATH="/opt/ci-tools/python/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi
# Controller and standalone jobs use one cache layout and environment setup.
if [ "${CI_DEPENDENCY_CACHE:-}" = 1 ]; then
 . /opt/ci/cache-env.sh
else
 export RUNNER_TOOL_CACHE=/home/runner/_work/_tool
 /opt/ci/toolcache-init.sh "$RUNNER_TOOL_CACHE"
 if [ -d /opt/ci-tools/python ]; then
  /opt/ci/cache-toolcache-init.sh "$RUNNER_TOOL_CACHE"
 fi
fi
# Atomic assignment gate shared with the controller's idle cancellation check.
# Job-writable metadata is advisory only; the controller enforces lifetime.
cat > /tmp/ci-job-started.sh <<'HOOK'
#!/bin/sh
set -eu
mkdir /tmp/ci-job-claimed
if [ "${CI_JOB_INPUTS:-}" = 1 ]; then
 python3 /opt/ci/job-input-gate.py
fi
HOOK
chmod 700 /tmp/ci-job-started.sh
export ACTIONS_RUNNER_HOOK_JOB_STARTED=/tmp/ci-job-started.sh
# The ephemeral job disk has no candidate state yet. Install the image-owned
# interposer before Listener receives its authenticated assignment. Later job
# steps can never reconnect to the already consumed controller socket.
if [ "${CI_JOB_INTAKE:-}" = 1 ]; then
 mv /home/runner/bin/Runner.Worker /home/runner/bin/Runner.Worker.real
 cat > /home/runner/bin/Runner.Worker <<'INTAKE'
#!/bin/sh
exec /usr/bin/python3 /opt/ci/worker_intake.py "$@"
INTAKE
 chmod 500 /home/runner/bin/Runner.Worker
 if [ "${CI_INTAKE_DIAGNOSTICS:-}" = 1 ]; then
  /usr/bin/python3 -I -c 'import os,select
try:
 if select.select([], [2], [], 0)[1]: os.write(2,b"ci-intake/v1 interposer_installed\n")
except (OSError,ValueError): pass' || :
 fi
fi
cd /home/runner
exec ./run.sh
