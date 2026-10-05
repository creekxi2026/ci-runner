#!/bin/sh
# Source inside a trusted job; the volume contains package-manager-native caches.
set -eu
test "$(id -u)" = 1001
test -f /opt/ci-cache/ready.json
export npm_config_cache="$HOME/.npm"
mkdir -p "$npm_config_cache"
if [ ! -e "$npm_config_cache/_cacache" ] && [ ! -L "$npm_config_cache/_cacache" ]; then
    ln -s /opt/ci-cache/npm/_cacache "$npm_config_cache/_cacache"
fi
test "$(readlink "$npm_config_cache/_cacache")" = /opt/ci-cache/npm/_cacache
export GOMODCACHE=/opt/ci-cache/gomod
export PIP_CACHE_DIR=/opt/ci-cache/pip
export npm_config_prefer_offline=true
export PATH="/opt/ci-tools/bin:$PATH"
if command -v go >/dev/null 2>&1; then
    ci_abi=${CI_CACHE_ABI:-$(. /etc/os-release; printf '%s%s' "$ID" "$VERSION_ID")}
    case "$ci_abi" in ''|*[!a-zA-Z0-9._-]*) echo 'Invalid CI_CACHE_ABI' >&2; return 1;; esac
    export GOCACHE="/opt/ci-cache/go-build/$(go env GOOS)-$(go env GOARCH)-$(go env GOVERSION)-$ci_abi"
    mkdir -p "$GOCACHE"
fi
if command -v node >/dev/null 2>&1; then
    export CI_JEST_CACHE_DIR="/opt/ci-cache/jest/node$(node -p 'process.versions.node.split(".")[0]')"
    mkdir -p "$CI_JEST_CACHE_DIR"
fi
# An imported Python distribution is optional, not an application requirement.
if [ -d /opt/ci-tools/python/lib ]; then
    export LD_LIBRARY_PATH="/opt/ci-tools/python/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi
export RUNNER_TOOL_CACHE="$HOME/_work/_tool"
sh /opt/ci/cache-toolcache-init.sh "$RUNNER_TOOL_CACHE"
unset ci_abi
