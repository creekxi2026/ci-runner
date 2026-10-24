package scaleset

// Local, optional diagnostic patch. No request, error string or callback is
// retained in the queue; only this closed value type can cross the boundary.
import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"runtime"
	"sync/atomic"
	"time"
)

type DiagnosticStage string

const (
	InitConfig          DiagnosticStage = "init_config"
	InitCredentials     DiagnosticStage = "init_credentials"
	InitClient          DiagnosticStage = "init_client"
	InitDisk            DiagnosticStage = "init_disk"
	EnsureSet           DiagnosticStage = "ensure_set"
	AuthRefresh         DiagnosticStage = "auth_refresh"
	AuthRegistration    DiagnosticStage = "auth_registration"
	AuthAdminConnection DiagnosticStage = "auth_admin_connection"
	SessionCreate       DiagnosticStage = "session_create"
	SessionRefresh      DiagnosticStage = "session_refresh"
	SessionClose        DiagnosticStage = "session_close"
	Bootstrap           DiagnosticStage = "bootstrap"
	Poll                DiagnosticStage = "poll"
	Scale               DiagnosticStage = "scale"
	Acquire             DiagnosticStage = "acquire"
	Ack                 DiagnosticStage = "ack"
	Demand              DiagnosticStage = "demand"
	Reconcile           DiagnosticStage = "reconcile"
	Start               DiagnosticStage = "start"
	Diagnostic          DiagnosticStage = "diagnostic"
)

func validStage(s DiagnosticStage) bool {
	switch s {
	case InitConfig, InitCredentials, InitClient, InitDisk, EnsureSet, AuthRefresh, AuthRegistration, AuthAdminConnection, SessionCreate, SessionRefresh, SessionClose, Bootstrap, Poll, Scale, Acquire, Ack, Demand, Reconcile, Start, Diagnostic:
		return true
	}
	return false
}

type DiagnosticIdentity struct {
	WindowID       string `json:"window_id"`
	SourceRevision string `json:"source_revision"`
	ImageDigest    string `json:"image_digest"`
	ScopeRef       string `json:"scope_ref"`
}
type DiagnosticCounts struct {
	Poll202  uint64 `json:"poll_202"`
	Poll200  uint64 `json:"poll_200"`
	Poll401  uint64 `json:"poll_401"`
	Messages uint64 `json:"messages"`
	Acquired uint64 `json:"acquired"`
	Demand   uint64 `json:"demand"`
	Started  uint64 `json:"started"`
	Dropped  uint64 `json:"dropped"`
}
type DiagnosticEvent struct {
	Schema string `json:"schema"`
	DiagnosticIdentity
	Seq            uint64           `json:"seq"`
	OpID           uint64           `json:"op_id"`
	ParentOpID     *uint64          `json:"parent_op_id"`
	UTC            string           `json:"utc"`
	Stage          DiagnosticStage  `json:"stage"`
	Phase          string           `json:"phase"`
	LogicalAttempt uint64           `json:"logical_attempt"`
	HTTPAttempt    *uint64          `json:"http_attempt"`
	DurationMS     *int64           `json:"duration_ms"`
	Outcome        string           `json:"outcome"`
	HTTPStatus     *int             `json:"http_status"`
	ErrorClass     string           `json:"error_class"`
	SessionRef     *string          `json:"session_ref"`
	Counts         DiagnosticCounts `json:"counts"`
	LossTotal      uint64           `json:"loss_total"`
}
type Diagnostics struct {
	identity  DiagnosticIdentity
	salt      [32]byte
	queue     chan DiagnosticEvent
	producers atomic.Int64
	stop      chan struct{}
	done      chan struct{}
	closing   atomic.Bool
	loss      atomic.Uint64
	ops       atomic.Uint64
	failed    atomic.Bool
}
type diagnosticKey struct{}
type diagnosticSessionKey struct{}
type diagnosticCall struct {
	d        *Diagnostics
	op       uint64
	parent   *uint64
	stage    DiagnosticStage
	attempt  uint64
	started  time.Time
	http     atomic.Uint64
	httpOpen atomic.Bool
	status   atomic.Int64
	session  *string
}

var diagID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var diagRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var diagDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func NewDiagnostics(identity DiagnosticIdentity, salt [32]byte, writer io.Writer, capacity int) (*Diagnostics, error) {
	if !diagID.MatchString(identity.WindowID) || !diagID.MatchString(identity.ScopeRef) || !diagRevision.MatchString(identity.SourceRevision) || !diagDigest.MatchString(identity.ImageDigest) || salt == [32]byte{} || writer == nil || capacity < 1 || capacity > 4096 {
		return nil, errors.New("invalid diagnostic input")
	}
	d := &Diagnostics{identity: identity, salt: salt, queue: make(chan DiagnosticEvent, capacity), stop: make(chan struct{}), done: make(chan struct{})}
	go d.write(writer)
	return d, nil
}
func WithDiagnostics(d *Diagnostics) HTTPOption {
	return func(o *httpClientOption) { o.diagnostics = d }
}
func (d *Diagnostics) Loss() uint64 {
	if d == nil {
		return 0
	}
	return d.loss.Load()
}
func (d *Diagnostics) enqueue(e DiagnosticEvent) {
	if d == nil {
		return
	}
	d.producers.Add(1)
	defer d.producers.Add(-1)
	if d.closing.Load() || d.failed.Load() {
		d.loss.Add(1)
		return
	}
	select {
	case d.queue <- e:
	default:
		d.loss.Add(1)
	}
}
func (d *Diagnostics) write(w io.Writer) {
	defer close(d.done)
	defer func() {
		if recover() != nil {
			d.failed.Store(true)
			d.loss.Add(1)
		}
	}()
	var seq uint64
	var total int
	emit := func(e DiagnosticEvent) bool {
		seq++
		e.Seq = seq
		e.Schema = "ci-stage-v1"
		e.DiagnosticIdentity = d.identity
		e.LossTotal = d.loss.Load()
		b, err := json.Marshal(e)
		if err != nil || len(b)+1 > 2048 {
			d.loss.Add(1)
			return true
		}
		b = append(b, '\n')
		if total+len(b) > 20*1024*1024 {
			d.loss.Add(1)
			d.failed.Store(true)
			return false
		}
		total += len(b)
		n, err := w.Write(b)
		if err != nil || n != len(b) {
			d.loss.Add(1)
			d.failed.Store(true)
			return false
		}
		return true
	}
	for {
		select {
		case e := <-d.queue:
			if !emit(e) {
				return
			}
		case <-d.stop:
			// Closing forbids future accepted events. Synchronize only the
			// final enqueue already in progress; producers never wait here.
			for d.producers.Load() != 0 {
				runtime.Gosched()
			}
			for {
				select {
				case e := <-d.queue:
					if !emit(e) {
						return
					}
				default:
					emit(DiagnosticEvent{OpID: d.ops.Add(1), UTC: time.Now().UTC().Format(time.RFC3339Nano), Stage: Diagnostic, Phase: "summary", LogicalAttempt: 1, Outcome: "dropped", ErrorClass: "none", Counts: DiagnosticCounts{Dropped: d.loss.Load()}})
					return
				}
			}
		}
	}
}

// Close never waits beyond the caller's budget. A physically blocked writer may
// remain until process exit; false means evidence is incomplete.
func (d *Diagnostics) Close(ctx context.Context) bool {
	if d == nil {
		return true
	}
	if d.closing.CompareAndSwap(false, true) {
		close(d.stop)
	}
	select {
	case <-d.done:
		return !d.failed.Load() && d.loss.Load() == 0
	case <-ctx.Done():
		d.loss.Add(1)
		return false
	}
}
func (d *Diagnostics) Begin(ctx context.Context, stage DiagnosticStage, attempt uint64) (context.Context, func(error)) {
	if d == nil {
		return ctx, func(error) {}
	}
	if !validStage(stage) || attempt < 1 {
		d.loss.Add(1)
		return ctx, func(error) {}
	}
	c := &diagnosticCall{d: d, op: d.ops.Add(1), stage: stage, attempt: attempt, started: time.Now()}
	if p, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall); p != nil && p.d == d {
		v := p.op
		c.parent = &v
		c.session = p.session
	}
	if ref, _ := ctx.Value(diagnosticSessionKey{}).(*string); ref != nil {
		c.session = ref
	}
	ctx = context.WithValue(ctx, diagnosticKey{}, c)
	d.enqueue(c.event("begin", "pending", "none", nil, nil))
	return ctx, func(err error) {
		class := DiagnosticErrorClass(err)
		outcome := "ok"
		if err != nil {
			outcome = "fatal"
		}
		if class == "cancelled" {
			outcome = "cancelled"
		}
		if class == "deadline" || class == "network_timeout" {
			outcome = "timeout"
		}
		ms := time.Since(c.started).Milliseconds()
		e := c.event("end", outcome, class, nil, nil)
		e.DurationMS = &ms
		if s := int(c.status.Load()); s >= 100 && s <= 599 {
			e.HTTPStatus = &s
		}
		d.enqueue(e)
	}
}
func (c *diagnosticCall) event(phase, outcome, class string, httpAttempt *uint64, status *int) DiagnosticEvent {
	return DiagnosticEvent{OpID: c.op, ParentOpID: c.parent, UTC: time.Now().UTC().Format(time.RFC3339Nano), Stage: c.stage, Phase: phase, LogicalAttempt: c.attempt, HTTPAttempt: httpAttempt, Outcome: outcome, ErrorClass: class, HTTPStatus: status, SessionRef: c.session}
}

// Session IDs never leave the SDK in raw form. The context copy does not change
// the parent operation or its lifecycle; it only associates subsequent events.
func (d *Diagnostics) sessionContext(ctx context.Context, id string) context.Context {
	if d == nil {
		return ctx
	}
	m := hmac.New(sha256.New, d.salt[:])
	_, _ = m.Write([]byte(id))
	ref := hex.EncodeToString(m.Sum(nil))
	return context.WithValue(ctx, diagnosticSessionKey{}, &ref)
}
func diagnosticHTTPBegin(ctx context.Context) {
	if c, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall); c != nil && c.httpOpen.CompareAndSwap(false, true) {
		n := c.http.Add(1)
		c.d.enqueue(c.event("begin", "pending", "none", &n, nil))
	}
}
func diagnosticHTTPEnd(ctx context.Context, status int, err error, retry bool) {
	if c, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall); c != nil && c.httpOpen.CompareAndSwap(true, false) {
		n := c.http.Load()
		var s *int
		if status >= 100 && status <= 599 {
			s = &status
			c.status.Store(int64(status))
		}
		class := DiagnosticErrorClass(err)
		outcome := "ok"
		if err != nil || status >= 400 {
			outcome = "fatal"
		}
		if retry {
			outcome = "retry"
		}
		if status == 202 {
			outcome = "empty"
		}
		if status >= 400 {
			class = diagnosticStatus(status)
		}
		if class == "cancelled" {
			outcome = "cancelled"
		}
		if class == "deadline" || class == "network_timeout" {
			outcome = "timeout"
		}
		e := c.event("end", outcome, class, &n, s)
		if c.stage == Poll {
			switch status {
			case 202:
				e.Counts.Poll202 = 1
			case 200:
				e.Counts.Poll200 = 1
			case 401:
				e.Counts.Poll401 = 1
			}
		}
		c.d.enqueue(e)
	}
}
func diagnosticStatus(s int) string {
	switch {
	case s == 401:
		return "http_unauthorized"
	case s == 403:
		return "http_forbidden"
	case s == 404:
		return "http_not_found"
	case s == 429:
		return "http_rate_limited"
	case s >= 500:
		return "http_server"
	default:
		return "unknown"
	}
}

// No Error(), formatting or string matching: unknown stays unknown.
func DiagnosticErrorClass(err error) (class string) {
	class = "unknown"
	defer func() {
		if recover() != nil {
			class = "unknown"
		}
	}()
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var record tls.RecordHeaderError
	if errors.As(err, &cert) || errors.As(err, &authority) || errors.As(err, &record) {
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "network_timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "connection"
	}
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) {
		return "decode"
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return "local_io"
	}
	if errors.Is(err, UnauthorizedError) || errors.Is(err, MessageQueueTokenExpiredError) {
		return "http_unauthorized"
	}
	if errors.Is(err, NotFoundError) {
		return "http_not_found"
	}
	return class
}

// Count accepts only fixed numeric keys; no caller-supplied strings enter output.
func (d *Diagnostics) Count(ctx context.Context, counts DiagnosticCounts) {
	if d == nil {
		return
	}
	if c, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall); c != nil && c.d == d {
		e := c.event("summary", "ok", "none", nil, nil)
		e.Counts = counts
		d.enqueue(e)
	}
}

// Called synchronously after create completes, before its deferred end event.
func (d *Diagnostics) associateSession(ctx context.Context, id string) {
	if d == nil {
		return
	}
	ref, _ := d.sessionContext(ctx, id).Value(diagnosticSessionKey{}).(*string)
	if c, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall); c != nil && c.d == d {
		c.session = ref
	}
}
