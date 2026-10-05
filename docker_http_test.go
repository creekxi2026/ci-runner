package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise serialization, HTTP cancellation and Docker status handling over a
// real local HTTP connection; no Docker daemon or external writes are involved.
func withDockerHTTP(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(h)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}}
	old := engine
	engine = &http.Client{Transport: transport, Timeout: time.Second * 5}
	t.Cleanup(func() { transport.CloseIdleConnections(); server.Close(); engine = old })
}
