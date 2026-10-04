package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Only the controller config can select a trusted upstream; jobs cannot.
func upstreamProxyEnv(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("invalid PUBLIC_EGRESS_UPSTREAM_PROXY: require credential-free HTTP proxy URL")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid PUBLIC_EGRESS_UPSTREAM_PROXY port")
		}
	}
	return []string{"PUBLIC_EGRESS_UPSTREAM_PROXY=" + value}, nil
}
