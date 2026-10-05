package main

import (
	"fmt"
	"net/url"
	"strings"
)

// Only deployment configuration selects DNS-over-HTTPS; workloads cannot.
func dohProxyEnv(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("invalid PUBLIC_EGRESS_DOH_URL: require credential-free HTTPS resolver URL on port 443 without query or fragment")
	}
	return []string{"PUBLIC_EGRESS_DOH_URL=" + value}, nil
}
