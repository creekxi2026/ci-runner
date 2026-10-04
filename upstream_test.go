package main

import "testing"

func TestUpstreamConfigurationIsAdminOnlyCredentialFree(t *testing.T) {
	for _, value := range []string{"https://gateway:7897", "http://user:pass@gateway:7897", "http://gateway:0", "http://gateway:65536", "http://gateway/path", "http://gateway?token=secret", "http://gateway#fragment", " http://gateway:7897"} {
		if _, err := upstreamProxyEnv(value); err == nil {
			t.Fatal("invalid proxy config accepted")
		}
	}
	for _, value := range []string{"http://host.docker.internal:7897", "http://[::1]:7897"} {
		v, err := upstreamProxyEnv(value)
		if err != nil || len(v) != 1 || v[0] != "PUBLIC_EGRESS_UPSTREAM_PROXY="+value {
			t.Fatalf("proxy config not preserved: %v", err)
		}
	}
	v, err := upstreamProxyEnv("")
	if err != nil || v != nil {
		t.Fatal("direct mode must be default")
	}
}
