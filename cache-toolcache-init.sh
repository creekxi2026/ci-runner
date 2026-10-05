#!/bin/sh
# Register installed tools without downloading or requiring a project profile.
set -eu
cache=$1
case $(uname -m) in aarch64) arch=arm64;; x86_64) arch=x64;; *) exit 1;; esac
seed() {
    target=$cache/$1/$2/$arch
    mkdir -p "$(dirname "$target")"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then ln -s "$3" "$target"; fi
    [ "$(readlink "$target")" = "$3" ]
    touch "$target.complete"
}
if command -v go >/dev/null 2>&1; then
    version=$(go env GOVERSION)
    seed go "${version#go}" "$(go env GOROOT)"
fi
if command -v node >/dev/null 2>&1; then
    node_bin=$(readlink -f "$(command -v node)")
    seed node "$(node -p process.versions.node)" "$(dirname "$(dirname "$node_bin")")"
fi
if [ -x /opt/ci-tools/python/bin/python3 ]; then
    seed Python "$(/opt/ci-tools/python/bin/python3 -c 'import platform; print(platform.python_version())')" /opt/ci-tools/python
fi
