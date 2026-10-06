#!/bin/bash
# Project code is an explicit external fixture input, never part of the runner.
# Requires Docker-produced immutable references; never turn an image ID into a
# fabricated repository digest. Local references do not imply registry release.
set -euo pipefail
base=$1
consumer_dir=$2
fixture="ci-consumer-check-$(date +%s)-$$"
test_dir=$(mktemp -d)
cleanup() {
  docker rm -f "$fixture" >/dev/null 2>&1 || true
  while read -r id; do [ -z "$id" ] || docker rm -f "$id" >/dev/null; done < <(docker ps -aq --filter "label=ci-runner.owner=$fixture")
  while read -r id; do [ -z "$id" ] || docker network rm "$id" >/dev/null; done < <(docker network ls -q --filter "label=ci-runner.owner=$fixture")
  docker volume rm "$fixture" >/dev/null 2>&1 || true
  docker image rm "$fixture-worker" "$fixture-harness" "$fixture-base" >/dev/null 2>&1 || true
  rm -rf "$test_dir"
}
trap cleanup EXIT
[[ "$base" =~ @sha256:[a-f0-9]{64}$ ]]
# BuildKit can resolve a local-only digest through Docker inspect but still try
# a registry for FROM. Give that exact local image a fixture-owned build alias;
# runtime admission and fingerprints use real RepoDigests, never this alias.
docker image inspect "$base" >/dev/null
docker tag "$base" "$fixture-base"
test -f "$consumer_dir/catalog.json"
test -f "$consumer_dir/policy.json"
test -f "$consumer_dir/probe.py"
cp tests/service-consumer/Dockerfile "$test_dir/Dockerfile"
cp service-firewall.sh firewall.sh job-disk-init.py ci-service.py "$test_dir/"
cp -R "$consumer_dir" "$test_dir/consumer"
revision=$(git rev-parse HEAD)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -ldflags="-X github.com/creekxi2026/ci-runner.imageRevision=$revision" -o "$test_dir/check"
for target in worker harness; do
  docker build --network none --pull=false --target "$target" --build-arg "BASE=$fixture-base" \
    --label "org.opencontainers.image.revision=$revision" -t "$fixture-$target" "$test_dir" \
    > "$test_dir/build-$target.log" 2>&1 || { tail -20 "$test_dir/build-$target.log"; exit 1; }
done
immutable_ref() {
  docker image inspect "$1" | python3 -c 'import json,sys; d=json.load(sys.stdin)[0]; refs=d.get("RepoDigests",[]); assert refs,"Docker did not return an immutable manifest reference"; print(refs[0])'
}
runner_ref=$(immutable_ref "$fixture-worker")
controller_ref=$(immutable_ref "$fixture-harness")
docker volume create "$fixture" >/dev/null
docker run --rm --name "$fixture" --network none --read-only --user 0 \
 --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
 --security-opt no-new-privileges --memory 512m --memory-swap 512m --cpus 1 \
 --mount "type=volume,src=$fixture,dst=/work" \
 --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
 -e CI_SERVICE_CONSUMER_TEST=1 -e "CI_SERVICE_TEST_OWNER=$fixture" \
 -e CI_SERVICE_TEST_ROOT=/work -e "CI_SERVICE_TEST_VOLUME=$fixture" \
 -e "RUNNER_IMAGE=$runner_ref" -e "CONTROLLER_IMAGE=$controller_ref" \
 -e "CONTROLLER_CONTAINER=$fixture" -e "IMAGE_REVISION=$revision" \
 "$controller_ref" -test.run='^TestServiceConsumerRuntime$' -test.v -test.timeout=180s
