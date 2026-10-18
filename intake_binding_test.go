package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestBindingPrepareRequiresPrivateStdin(t *testing.T) {
	f, b := inputFixture(t, "cat >/dev/null; exit 0")
	b.policy.Schema = "ci-job-inputs/v2"
	t.Cleanup(b.closeIntakes)
	spec := inputSpec()
	if err := f.prepareJobInputs(context.Background(), "ci-job-bound", spec); err != nil {
		t.Fatal(err)
	}
	if spec["OpenStdin"] != true || spec["StdinOnce"] != true || spec["AttachStdin"] != true {
		t.Fatal("private controller bootstrap channel absent")
	}
	for _, e := range spec["Env"].([]string) {
		if len(e) > 0 && e[:min(len(e), 18)] == "CI_INTAKE_SECRET=" {
			t.Fatal("material entered spec")
		}
	}
}

func TestBindingUnboundPacketCannotStartHelper(t *testing.T) {
	_, b := inputFixture(t, "cat >/dev/null; printf 0; sleep 10")
	b.policy.Schema = "ci-job-inputs/v2"
	name := "ci-job-bound"
	if err := b.root.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := b.root.Mkdir(name+"/intake", 0755); err != nil {
		t.Fatal(err)
	}
	if err := b.write(name+"/assignment.json", obj{}, true); err != nil {
		t.Fatal(err)
	}
	if err := b.startIntake(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	defer b.closeIntakes()
	d, err := b.root.Open(name + "/intake")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	address := fmt.Sprintf("/proc/self/fd/%d/session.sock", d.Fd())
	conn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write([]byte("{\"schema\":\"ci-job-intake/v1\",\"message\":{}}\n"))
	var response [1]byte
	n, _ := conn.Read(response[:])
	if n > 0 {
		t.Fatal("unbound ordinary client reached helper readiness")
	}
	// Single-use refusal leaves no named socket for a second connection.
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		if _, err := os.Stat(address); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("refused socket remained reconnectable")
}

// Test utility for fixtures that explicitly supply the trusted issuer side.
func testAuthenticateIntake(t *testing.T, conn net.Conn, material []byte) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if n, err := conn.Write(material); err != nil || n != 32 {
		t.Fatal("fixture bootstrap write failed")
	}
	var ack [1]byte
	if n, err := conn.Read(ack[:]); err != nil || n != 1 || ack[0] != '1' {
		t.Fatal("fixture binding rejected")
	}
	conn.SetDeadline(time.Time{})
}

func TestBindingLifecycleRejectsForeignStaleAndExpired(t *testing.T) {
	for _, kind := range []string{"valid", "wrong-allocation", "expired", "canceled", "reused"} {
		t.Run(kind, func(t *testing.T) {
			material, err := newIntakeMaterial()
			if err != nil {
				t.Fatal("material unavailable")
			}
			b := &jobInputs{intakes: intakeState{material: map[string]*intakeMaterial{"ci-job-bound": material}}}
			value, err := b.takeBootstrap("ci-job-bound")
			if err != nil {
				t.Fatal("issuance unavailable")
			}
			defer clear(value)
			if _, err := b.takeBootstrap("ci-job-bound"); err == nil {
				t.Fatal("material issued twice")
			}
			if kind == "wrong-allocation" {
				other, err := newIntakeMaterial()
				if err != nil {
					t.Fatal("other material unavailable")
				}
				copy(value, other.verify)
				other.clear()
			}
			if kind == "expired" {
				material.expires = time.Now().Add(-time.Second)
			}
			if kind == "canceled" || kind == "reused" {
				b.eraseBootstrap("ci-job-bound")
			}
			host, client := net.Pipe()
			defer client.Close()
			done := make(chan bool, 1)
			go func() { defer host.Close(); done <- b.bindIntake(context.Background(), "ci-job-bound", host) }()
			client.SetDeadline(time.Now().Add(time.Second))
			client.Write(value)
			var ack [1]byte
			n, _ := client.Read(ack[:])
			accepted := <-done
			if accepted != (kind == "valid") || (n == 1) != (kind == "valid") {
				t.Fatal("binding accepted wrong lifecycle")
			}
			if len(b.intakes.material) != 0 {
				t.Fatal("spent material retained")
			}
			for _, v := range append(material.issue, material.verify...) {
				if v != 0 {
					t.Fatal("spent material not erased")
				}
			}
		})
	}
}

func TestBindingCancellationWhileAuthenticationIsPrompt(t *testing.T) {
	material, err := newIntakeMaterial()
	if err != nil {
		t.Fatal("material unavailable")
	}
	b := &jobInputs{intakes: intakeState{material: map[string]*intakeMaterial{"ci-job-bound": material}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host, client := net.Pipe()
	defer client.Close()
	done := make(chan bool, 1)
	go func() { defer host.Close(); done <- b.bindIntake(ctx, "ci-job-bound", host) }()
	if _, err := client.Write([]byte{0}); err != nil {
		t.Fatal("partial authentication unavailable")
	}
	cancel()
	select {
	case accepted := <-done:
		if accepted {
			t.Fatal("canceled binding accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation retained authentication")
	}
}

func TestBindingUpgradeRemainsDuplexWithEngineTimeout(t *testing.T) {
	done := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.47/containers/ci-job-bound/attach" || r.URL.Query().Get("stdin") != "1" {
			t.Error("wrong bootstrap channel")
			w.WriteHeader(400)
			return
		}
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error("upgrade failed")
			return
		}
		defer conn.Close()
		buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		buffer.Flush()
		var marker [1]byte
		_, err = io.ReadFull(conn, marker[:])
		done <- err == nil && marker[0] == 'x'
	}))
	defer server.Close()
	previous := engine
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	engine = &http.Client{Transport: transport, Timeout: time.Second}
	t.Cleanup(func() { engine = previous; transport.CloseIdleConnections() })
	f := &fleet{inputs: &jobInputs{policy: inputPolicy{Schema: "ci-job-inputs/v2"}}}
	stream, err := f.attachBootstrap(context.Background(), "ci-job-bound")
	if err != nil {
		t.Fatal("engine timeout hid upgraded write interface")
	}
	if _, err := stream.Write([]byte{'x'}); err != nil {
		t.Fatal("upgraded stdin is not writable")
	}
	stream.Close()
	if !<-done {
		t.Fatal("upgraded byte not received")
	}
}

func TestBindingRestartCannotRecoverSpentAuthority(t *testing.T) {
	_, b := inputFixture(t, "exit 0")
	b.policy.Schema = "ci-job-inputs/v2"
	name := "ci-job-restart"
	if err := b.root.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := b.root.Mkdir(name+"/intake", 0755); err != nil {
		t.Fatal(err)
	}
	if err := b.startIntake(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	material, err := b.takeBootstrap(name)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(material)
	b.closeIntakes()
	// A fresh controller has neither the old issuer nor verifier, even with the
	// retained allocation directory. It must not reconstruct material from disk.
	restarted := &jobInputs{root: b.root, policy: b.policy}
	if _, err := restarted.takeBootstrap(name); err == nil {
		t.Fatal("restart recovered authority")
	}
	if err := restarted.startIntake(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	defer restarted.closeIntakes()
	d, err := b.root.Open(name + "/intake")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	conn, err := net.Dial("unix", fmt.Sprintf("/proc/self/fd/%d/session.sock", d.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write(material)
	var ack [1]byte
	if n, _ := conn.Read(ack[:]); n != 0 {
		t.Fatal("prior controller material accepted")
	}
}
