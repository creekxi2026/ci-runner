package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const diagnosticLimit = 512 * 1024

type proxyDiagnostic struct {
	Event          string   `json:"event"`
	ClosedAt       string   `json:"closed_at"`
	Job            string   `json:"job"`
	Hostname       string   `json:"hostname,omitempty"`
	PeerIP         string   `json:"peer_ip,omitempty"`
	Port           int      `json:"port"`
	Phase          string   `json:"phase"`
	ErrorType      string   `json:"error_type,omitempty"`
	CloseReason    string   `json:"close_reason"`
	CloseDirection string   `json:"close_direction"`
	EOF            []string `json:"eof_directions"`
	Duration       float64  `json:"duration_seconds"`
	LastIO         float64  `json:"last_io_seconds"`
	Sent           int64    `json:"bytes_client_to_server"`
	Received       int64    `json:"bytes_server_to_client"`
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

var diagnosticHost = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func sanitizeDiagnostic(line []byte, job string, closedAt string) ([]byte, bool) {
	var d proxyDiagnostic
	if len(line) > 4096 || json.Unmarshal(line, &d) != nil || d.Event != "egress_close" {
		return nil, false
	}
	if !oneOf(d.Phase, "request", "policy", "resolve", "dial", "upstream_handshake", "response", "relay") ||
		!oneOf(d.CloseReason, "eof", "idle_timeout", "error") || !oneOf(d.CloseDirection, "client", "server", "both") ||
		!oneOf(d.ErrorType, "", "TimeoutError", "ConnectionResetError", "BrokenPipeError", "OSError", "ValueError", "UnicodeDecodeError", "UnicodeEncodeError", "gaierror", "URLError", "HTTPError") ||
		(d.Port != 0 && d.Port != 80 && d.Port != 443) || d.Sent < 0 || d.Received < 0 ||
		math.IsNaN(d.Duration) || math.IsInf(d.Duration, 0) || d.Duration < 0 || d.Duration > 86400 ||
		d.LastIO < 0 || d.LastIO > d.Duration || len(d.EOF) > 2 {
		return nil, false
	}
	for _, direction := range d.EOF {
		if !oneOf(direction, "client", "server") {
			return nil, false
		}
	}
	if len(d.Hostname) > 253 {
		return nil, false
	}
	if d.Hostname != "" {
		for _, label := range strings.Split(strings.TrimSuffix(d.Hostname, "."), ".") {
			if !diagnosticHost.MatchString(label) {
				return nil, false
			}
		}
	}
	if d.PeerIP != "" && net.ParseIP(d.PeerIP) == nil {
		return nil, false
	}
	d.Job = job           // Never trust a job identity supplied in a log record.
	d.ClosedAt = closedAt // Docker's timestamp, not a field supplied by the payload.
	d.Event = "retained_egress_close"
	data, err := json.Marshal(d) // Unknown fields and all raw text are discarded.
	return data, err == nil
}

// Docker non-TTY logs consist of 8-byte framing headers and arbitrary chunks;
// records may span chunks. Reject malformed/oversized streams without echoing.
func decodeProxyLogs(r io.Reader, job string, emit func([]byte)) (int, error) {
	var payload bytes.Buffer
	limited := io.LimitReader(r, diagnosticLimit+1)
	for {
		var header [8]byte
		_, err := io.ReadFull(limited, header[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		n := int64(binary.BigEndian.Uint32(header[4:]))
		if (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 || n > 8192 || int64(payload.Len())+n > diagnosticLimit {
			return 0, fmt.Errorf("invalid diagnostic framing")
		}
		if _, err := io.CopyN(&payload, limited, n); err != nil {
			return 0, err
		}
	}
	count := 0
	for _, line := range bytes.Split(payload.Bytes(), []byte{'\n'}) {
		parts := bytes.SplitN(line, []byte{' '}, 2)
		if len(parts) != 2 {
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, string(parts[0]))
		if err != nil {
			continue
		}
		if data, ok := sanitizeDiagnostic(parts[1], job, stamp.UTC().Format(time.RFC3339Nano)); ok {
			emit(data)
			count++
			if count == 200 {
				break
			}
		}
	}
	return count, nil
}

func captureProxyDiagnostics(job string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/v1.47/containers/"+url.PathEscape(job+"-proxy")+"/logs?stdout=false&stderr=true&tail=200&timestamps=true", nil)
	if err != nil {
		return
	}
	resp, err := engine.Do(req)
	if err != nil {
		log.Printf("proxy diagnostics job=%s unavailable", job)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("proxy diagnostics job=%s status=%d", job, resp.StatusCode)
		return
	}
	count, err := decodeProxyLogs(resp.Body, job, func(record []byte) { log.Print(string(record)) })
	if err != nil {
		log.Printf("proxy diagnostics job=%s incomplete", job)
		return
	}
	log.Printf("proxy diagnostics job=%s retained=%d (tail limit=200)", job, count)
}
