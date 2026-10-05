package main

import "testing"

func TestDohProxyEnv(t *testing.T) {
	for _, v := range []string{"", "https://dns.alidns.com/resolve", "https://dns.google/resolve"} {
		env, err := dohProxyEnv(v)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if v == "" {
			if len(env) != 0 {
				t.Fatal("empty resolver must retain system DNS")
			}
		} else if len(env) != 1 || env[0] != "PUBLIC_EGRESS_DOH_URL="+v {
			t.Fatalf("unexpected env %v", env)
		}
	}
	for _, v := range []string{"http://dns.alidns.com/resolve", "https://u:p@dns.alidns.com/resolve", "https://dns.alidns.com/resolve?name=other", "https://dns.alidns.com/resolve#fragment", "https://dns.alidns.com:8443/resolve", " https://dns.alidns.com/resolve", "https:///resolve"} {
		if _, err := dohProxyEnv(v); err == nil {
			t.Fatalf("accepted unsafe resolver %q", v)
		}
	}
}
