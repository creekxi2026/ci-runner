#!/bin/bash
# Uses a fresh, uniquely named volume and only fixture-owned containers/networks.
set -euo pipefail
runner_ref=$1
fixture="ci-service-check-$(date +%s)-$$"
test_dir=$(mktemp -d)
cleanup() {
  docker rm -f "$fixture" >/dev/null 2>&1 || true
  while read -r id; do [ -z "$id" ] || docker rm -f "$id" >/dev/null; done < <(docker ps -aq --filter "label=ci-runner.owner=$fixture")
  while read -r id; do [ -z "$id" ] || docker network rm "$id" >/dev/null; done < <(docker network ls -q --filter "label=ci-runner.owner=$fixture")
  docker volume rm "$fixture" >/dev/null 2>&1 || true
  docker image rm "$fixture" >/dev/null 2>&1 || true
  rm -rf "$test_dir"
}
trap cleanup EXIT
docker build --network none --pull=false --build-arg "BASE=$runner_ref" -f tests/service-fixture/Dockerfile -t "$fixture" . > "$test_dir/build.log" 2>&1 || { tail -20 "$test_dir/build.log"; exit 1; }
image_id=$(docker image inspect "$fixture" --format '{{.Id}}')
docker volume create "$fixture" >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o "$test_dir/check"
docker run --rm --name "$fixture" --network none --read-only --user 0 \
 --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
 --security-opt no-new-privileges --memory 512m --memory-swap 512m --cpus 1 \
 --mount "type=volume,src=$fixture,dst=/work" \
 --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
 --mount "type=bind,src=$test_dir/check,dst=/test,readonly" \
 -e "CI_SERVICE_TEST_OWNER=$fixture" -e CI_SERVICE_TEST_ROOT=/work -e "CI_SERVICE_TEST_VOLUME=$fixture" \
 -e "CI_SERVICE_TEST_IMAGE=$image_id" \
 --entrypoint /test "$runner_ref" -test.run='^TestServiceRuntime$' -test.v -test.timeout=240s
