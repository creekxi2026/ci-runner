#!/bin/bash
# Isolated lifecycle fixture, never a production cache or workspace.
set -euo pipefail
runner_ref=$1
postgres_ref=$2
fixture="ci-work-check-$(date +%s)-$$"
test_dir=$(mktemp -d)
cleanup() {
  docker rm -f "$fixture" >/dev/null 2>&1 || true
  for owner in "$fixture-a" "$fixture-b"; do
    while read -r id; do
      [ -z "$id" ] || docker rm -f "$id" >/dev/null
    done < <(docker ps -aq --filter "label=ci-runner.owner=$owner")
  done
  docker volume rm "$fixture" >/dev/null
  rm -rf "$test_dir"
}
trap cleanup EXIT
docker volume create "$fixture" >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o "$test_dir/check"
docker run --rm --name "$fixture" --label com.docker.compose.project=ci-validation --label com.docker.compose.service=pool-fixture --network none --read-only --user 0 \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --security-opt no-new-privileges --memory 256m --memory-swap 256m --cpus 1 \
  --mount "type=volume,src=$fixture,dst=/work" \
  --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
  --mount "type=bind,src=$test_dir/check,dst=/test,readonly" \
  -e "CI_WORK_TEST_OWNER_PREFIX=$fixture" -e CI_WORK_TEST_ROOT=/work -e "CI_WORK_TEST_VOLUME=$fixture" \
  -e "CI_DISK_INTEGRATION_IMAGE=$runner_ref" -e "CI_DISK_INTEGRATION_POSTGRES=$postgres_ref" \
  --entrypoint /test "$runner_ref" -test.run='^TestSharedWorkRuntime$' -test.v -test.timeout=120s
