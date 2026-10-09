#!/bin/sh
set -eu
cache=$1
go_root=${2:-/opt/go}
node_root=${3:-/usr/local}
seed() {
 target=$cache/$1/$2/arm64
 mkdir -p "$(dirname "$target")"
 if [ ! -e "$target" ] && [ ! -L "$target" ]; then ln -s "$3" "$target"; fi
 [ "$(readlink "$target")" = "$3" ]
 touch "$target.complete"
}
seed go 1.26.9 "$go_root"
seed node 24.14.0 "$node_root"
