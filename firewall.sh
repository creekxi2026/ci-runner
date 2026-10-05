#!/bin/sh
set -eu
for cmd in iptables ip6tables; do
 "$cmd" -F INPUT
 "$cmd" -P INPUT DROP
 "$cmd" -A INPUT -i lo -j ACCEPT
 "$cmd" -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
 "$cmd" -F OUTPUT
 "$cmd" -P OUTPUT DROP
 "$cmd" -A OUTPUT -o lo -j ACCEPT
 "$cmd" -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
done
iptables -A OUTPUT -d "$1" -p tcp --dport 3128 -j ACCEPT
if [ -n "${2:-}" ]; then
 iptables -A OUTPUT -d "$2" -p tcp --dport 5432 -j ACCEPT
fi
# Job has no CAP_NET_ADMIN; helper exits before runner registration.
