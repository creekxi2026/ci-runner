package main

// Allocation material exists only in controller/broker/interposer memory and
// the existing private Docker stdin stream. Never put it into spec/env/files.
import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type intakeMaterial struct {
	issue, verify []byte
	expires       time.Time
}

func (m *intakeMaterial) clear() { clear(m.issue); clear(m.verify) }

func (b *jobInputs) takeBootstrap(name string) ([]byte, error) {
	b.intakes.mu.Lock()
	defer b.intakes.mu.Unlock()
	m := b.intakes.material[name]
	if m == nil || len(m.issue) != 32 || !time.Now().Before(m.expires) {
		return nil, fmt.Errorf("bootstrap unavailable")
	}
	value := append([]byte(nil), m.issue...)
	clear(m.issue)
	m.issue = nil
	return value, nil
}

// The caller holds intakeState.mu. The accepted connection consumes authority
// even on refusal. No error or retry can recreate this allocation's material.
func newIntakeMaterial() (*intakeMaterial, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("bootstrap unavailable")
	}
	return &intakeMaterial{issue: append([]byte(nil), value...), verify: value, expires: time.Now().Add(600 * time.Second)}, nil
}
func (b *jobInputs) eraseBootstrap(name string) {
	b.intakes.mu.Lock()
	defer b.intakes.mu.Unlock()
	if m := b.intakes.material[name]; m != nil {
		m.clear()
		delete(b.intakes.material, name)
	}
}
func (b *jobInputs) bindIntake(ctx context.Context, name string, conn net.Conn) bool {
	b.intakes.mu.Lock()
	m := b.intakes.material[name]
	var expected []byte
	var expires time.Time
	if m != nil {
		expires = m.expires
		expected = append([]byte(nil), m.verify...)
		m.clear()
		delete(b.intakes.material, name)
	}
	b.intakes.mu.Unlock()
	defer clear(expected)
	if len(expected) != 32 || ctx.Err() != nil || !time.Now().Before(expires) {
		return false
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(30 * time.Second)
	if expires.Before(deadline) {
		deadline = expires
	}
	conn.SetReadDeadline(deadline)
	received := make([]byte, 32)
	defer clear(received)
	if _, err := io.ReadFull(conn, received); err != nil || subtle.ConstantTimeCompare(expected, received) != 1 || ctx.Err() != nil || !time.Now().Before(expires) {
		return false
	}
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{'1'}); err != nil {
		return false
	}
	conn.SetWriteDeadline(time.Time{})
	return true
}

// Docker's HTTP Upgrade owns an interactive stdin stream before container start.
// The API identity and container selection are the existing controller boundary.
type bootstrapStream struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (s *bootstrapStream) Close() error {
	s.cancel()
	return s.ReadWriteCloser.Close()
}

func (f *fleet) attachBootstrap(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	if f.inputs == nil || f.inputs.policy.Schema != "ci-job-inputs/v2" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	owned := false
	defer func() {
		if !owned {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/v1.47/containers/"+url.PathEscape(name)+"/attach?stream=1&stdin=1&stdout=0&stderr=0", nil)
	if err != nil {
		return nil, fmt.Errorf("bootstrap unavailable")
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	// Client.Timeout wraps Body in a read-only interface, including for 101.
	// The request context bounds this private upgraded stream instead.
	client := *engine
	client.Timeout = 0
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bootstrap unavailable")
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body.Close()
		return nil, fmt.Errorf("bootstrap unavailable status=%d", response.StatusCode)
	}
	stream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		response.Body.Close()
		return nil, fmt.Errorf("bootstrap unavailable")
	}
	owned = true
	return &bootstrapStream{ReadWriteCloser: stream, cancel: cancel}, nil
}
func (b *jobInputs) sendBootstrap(name string, stream io.Writer) error {
	material, err := b.takeBootstrap(name)
	if err != nil {
		return err
	}
	defer clear(material)
	// A partial write fails closed; the channel is closed immediately afterward.
	if n, err := stream.Write(material); err != nil || n != len(material) {
		return fmt.Errorf("bootstrap unavailable")
	}
	return nil
}
