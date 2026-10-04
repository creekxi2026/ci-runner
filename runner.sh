#!/bin/sh
set -eu
n=0
while [ ! -e /tmp/ci-network-ready ]; do
 n=$((n+1)); [ "$n" -lt 120 ] || exit 1
 sleep 1
done
cd /home/runner
exec ./run.sh
