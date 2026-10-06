#!/bin/sh
set -eu
role=$1
peer=$2
shift 2
if [ "$role" = server ]; then
 for command in iptables ip6tables; do
  "$command" -w -F INPUT
  "$command" -w -P INPUT DROP
  "$command" -w -A INPUT -i lo -j ACCEPT
  "$command" -w -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  "$command" -w -F OUTPUT
  "$command" -w -P OUTPUT DROP
  if [ "$command" = iptables ]; then
   "$command" -w -A OUTPUT -d 127.0.0.11 -j DROP
  fi
  "$command" -w -A OUTPUT -o lo -j ACCEPT
  "$command" -w -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
 done
 for port do iptables -w -A INPUT -s "$peer" -p tcp --dport "$port" -j ACCEPT; done
elif [ "$role" = worker ]; then
 for port do
  iptables -w -C OUTPUT -d "$peer" -p tcp --dport "$port" -j ACCEPT 2>/dev/null ||
   iptables -w -A OUTPUT -d "$peer" -p tcp --dport "$port" -j ACCEPT
 done
else
 exit 64
fi
