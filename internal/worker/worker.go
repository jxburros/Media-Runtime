// Package worker is the Media Engine client: HTTP calls with the worker credential, protocol
// negotiation, supervision of an owned child process, and mutual TLS with certificate pinning
// for a remote worker.
package worker

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/transport"
)

// Trust is where a remote worker's certificate fingerprint is kept between runs: the one seen on
// first contact (or the configured pin), recorded so that every later connection must present it.
type Trust interface {
	CertificateFingerprint(workerID string) string
	RecordCertificateFingerprint(workerID, url, sha256 string) error
}

// Transport describes how the client reaches its worker, for diagnostics.
type Transport struct {
	// Scheme is "http" (loopback) or "https" (mutual TLS to a remote worker).
	Scheme string
	Remote bool
	// Pinned is true when the expected fingerprint is the configured pinned_sha256.
	Pinned bool
	// Expected is the fingerprint every connection must present ("" before first contact).
	Expected string
	// Presented is the fingerprint of the certificate the worker last presented.
	Presented string
	// Latency is the round trip of the last /v1/info request (0 before the first).
	Latency time.Duration
}

// Probe is one consistent view of the worker: info, capabilities and the negotiated minor.
type Probe struct {
	Info     *jsonv.Object
	Caps     *jsonv.Object
	Protocol string
}

// Child is an owned worker process.
type Child struct {
	Cmd    *exec.Cmd
	exited chan struct{}
	stdin  io.WriteCloser
}

// Exited reports whether the process has ended.
func (ch *Child) Exited() bool {
	select {
	case <-ch.exited:
		return true
	default:
		return false
	}
}

// Kill sends a signal to the process (tests use SIGKILL to simulate a crash).
func (ch *Child) Kill(sig syscall.Signal) { _ = ch.Cmd.Process.Signal(sig) }

type Client struct {
	// ID is the configured worker_id this client speaks to.
	ID    string
	cfg   *config.Config
	entry config.Worker
	mu    sync.Mutex
	url   string
	tok   string
	disc  string
	// Protocol is the minor negotiated with the current worker session; set by Probe.
	Protocol string
	child    *Child
	http     *http.Client

	// Remote workers: a configuration error found at construction (reported by Start), the
	// configured pin, the recorded fingerprint and the one the worker last presented.
	setupErr  error
	trust     Trust
	pinned    string
	recorded  string
	presented string
	latency   time.Duration

	hookJSON  func(path, method string, body any, result any) (any, error)
	hookProbe func(p *Probe) (*Probe, error)
}

// SetHooks lets tests observe or alter worker responses in process; nil clears a hook.
func (w *Client) SetHooks(json func(path, method string, body any, result any) (any, error), probe func(p *Probe) (*Probe, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hookJSON, w.hookProbe = json, probe
}

func (w *Client) hooks() (func(string, string, any, any) (any, error), func(*Probe) (*Probe, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hookJSON, w.hookProbe
}

// New is the client of one configured worker; the runtime keeps one per pool entry. A remote
// entry gets its own transport carrying the client certificate, the CA and the fingerprint check;
// every request to that worker, polling and streaming alike, goes through it.
func New(cfg *config.Config, entry config.Worker) *Client {
	timeout := time.Duration(cfg.WorkerTimeoutMS) * time.Millisecond
	if entry.Remote && entry.RemoteTimeoutMS > 0 {
		timeout = time.Duration(entry.RemoteTimeoutMS) * time.Millisecond
	}
	w := &Client{
		ID:       entry.WorkerID,
		cfg:      cfg,
		entry:    entry,
		url:      strings.TrimSuffix(entry.URL, "/"),
		tok:      os.Getenv(entry.TokenEnv),
		Protocol: c.PROTOCOLS[0],
	}
	t := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	if entry.Remote {
		config, err := w.tlsConfig()
		if err != nil {
			w.setupErr = err
		}
		t.TLSClientConfig = config
		t.ForceAttemptHTTP2 = false
	}
	w.http = &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are refused") },
		Transport:     t,
	}
	return w
}

// tlsConfig is the remote worker's client side of mutual TLS: TLS 1.3 (Engine's network mode
// minimum), the configured CA as the only trust root, the runtime's client certificate, and a
// fingerprint check on every handshake. Session resumption stays off (no ClientSessionCache),
// so VerifyPeerCertificate runs on every connection.
func (w *Client) tlsConfig() (*tls.Config, error) {
	material := w.entry.TLS
	if material == nil {
		return nil, fmt.Errorf("Worker %s: an https:// worker needs tls", w.ID)
	}
	w.pinned = material.PinnedSHA256
	label := "workers." + w.ID + ".tls"
	roots, err := transport.CertPool(material.CAFile, label+".ca_file")
	if err != nil {
		return nil, err
	}
	pair, err := transport.KeyPair(material.CertFile, material.KeyFile, label)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      roots,
		Certificates: []tls.Certificate{pair},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("worker presented no certificate")
			}
			presented := transport.Fingerprint(raw[0])
			w.mu.Lock()
			defer w.mu.Unlock()
			w.presented = presented
			if expected := w.expectedLocked(); expected != "" && presented != expected {
				return &transport.MismatchError{Expected: expected, Presented: presented}
			}
			return nil
		},
	}, nil
}

func (w *Client) expectedLocked() string {
	if w.pinned != "" {
		return w.pinned
	}
	return w.recorded
}

// SetTrust loads the recorded fingerprint of a remote worker and keeps the store for recording
// it on first contact.
func (w *Client) SetTrust(t Trust) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trust = t
	if w.entry.Remote && t != nil {
		w.recorded = t.CertificateFingerprint(w.ID)
	}
}

// Transport reports the scheme, the expected and presented fingerprints and the last latency.
func (w *Client) Transport() Transport {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := Transport{Scheme: "http", Remote: w.entry.Remote, Latency: w.latency}
	if w.entry.Remote {
		t.Scheme = "https"
		t.Pinned = w.pinned != ""
		t.Expected = w.expectedLocked()
		t.Presented = w.presented
	}
	return t
}

// Entry is the worker's configuration entry.
func (w *Client) Entry() config.Worker { return w.entry }

// Child returns the owned process, or nil for an externally managed worker.
func (w *Client) Child() *Child {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.child
}

// ClearChild forgets an exited child before a restart.
func (w *Client) ClearChild() {
	w.mu.Lock()
	w.child = nil
	w.mu.Unlock()
}

// DiscoveredSession is the session the owned child announced in its startup record.
func (w *Client) DiscoveredSession() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.disc
}

// Start launches the configured worker command, or checks the external worker credential (and,
// for a remote worker, that its TLS material loaded). A remote worker is never launched.
func (w *Client) Start() error {
	if w.setupErr != nil {
		return w.setupErr
	}
	command := w.entry.Command
	if len(command) == 0 || w.entry.Remote {
		if len(w.token()) < 32 {
			return errors.New("Set the configured worker token environment variable")
		}
		return nil
	}
	token := c.RandomHex(32)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "MEDIA_ENGINE_TOKEN="+token, "MEDIA_ENGINE_SUPERVISED=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	child := &Child{Cmd: cmd, exited: make(chan struct{}), stdin: stdin}
	w.mu.Lock()
	w.child = child
	w.tok = token
	w.mu.Unlock()
	type result struct {
		line []byte
		err  error
	}
	lines := make(chan result, 1)
	reader := bufio.NewReaderSize(stdout, 16384)
	go func() {
		var buffer []byte
		for {
			b, err := reader.ReadByte()
			if err != nil {
				lines <- result{err: errors.New("Worker exited before startup")}
				return
			}
			if b == '\n' {
				lines <- result{line: buffer}
				return
			}
			buffer = append(buffer, b)
			if len(buffer) > 8192 {
				lines <- result{err: errors.New("Worker startup record too large")}
				return
			}
		}
	}()
	timer := time.NewTimer(time.Duration(w.cfg.WorkerTimeoutMS) * time.Millisecond)
	defer timer.Stop()
	var line []byte
	select {
	case r := <-lines:
		err, line = r.err, r.line
	case <-timer.C:
		err = errors.New("Worker startup timed out")
	}
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		go func() { _ = cmd.Wait(); close(child.exited) }()
		return err
	}
	// Drain stdout so the child never blocks on a full pipe; stderr is never retained.
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		_ = cmd.Wait()
		close(child.exited)
	}()
	parsed, perr := jsonv.Parse(line)
	record, _ := parsed.(*jsonv.Object)
	if perr != nil || record == nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return errors.New("Invalid worker startup record")
	}
	pid, _ := record.Num("pid")
	port, portOK := record.Num("port")
	_, sessionOK := record.Get("session_id").(string)
	if !c.Contains(c.PROTOCOLS, record.Str("protocol")) || int(pid) != cmd.Process.Pid ||
		!portOK || port != float64(int(port)) || port < 1 || port > 65535 || !sessionOK {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return errors.New("Incompatible worker startup record")
	}
	w.mu.Lock()
	w.url = fmt.Sprintf("http://127.0.0.1:%d", int(port))
	w.disc = record.Str("session_id")
	w.mu.Unlock()
	return nil
}

func (w *Client) token() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tok
}

func (w *Client) base() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.url
}

var codePattern = regexp.MustCompile(`^[a-z_]{1,60}$`)

// Response performs one request. body is nil for none, []byte for raw bytes, or a JSON value.
func (w *Client) Response(path, method string, body any, contentType string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		if raw, ok := body.([]byte); ok {
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(jsonv.MustMarshal(body))
		}
	}
	req, err := http.NewRequest(method, w.base()+path, reader)
	if err != nil {
		return nil, c.Err(503, "backend_unavailable", "Worker is unavailable; check its process and configuration")
	}
	req.Header.Set("Authorization", "Bearer "+w.token())
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	res, err := w.http.Do(req)
	if err != nil {
		return nil, w.transportError(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		code := "worker_error"
		if parsed, err := jsonv.Parse(data); err == nil {
			if o, ok := parsed.(*jsonv.Object); ok {
				if s := o.Obj("error").Str("code"); codePattern.MatchString(s) {
					code = s
				}
			}
		}
		return nil, c.Err(res.StatusCode, code, "Worker reported "+strings.ReplaceAll(code, "_", " "))
	}
	return res, nil
}

// transportError explains a failed request. A certificate that is not the pinned or recorded one
// is certificate_mismatch; other TLS failures name the handshake; the rest is unavailability.
func (w *Client) transportError(err error) error {
	var mismatch *transport.MismatchError
	if errors.As(err, &mismatch) {
		return c.Err(503, "certificate_mismatch", fmt.Sprintf("Worker %s presented certificate sha256 %s, not the %s %s; the connection was refused", w.ID, mismatch.Presented, w.expectedSource(), mismatch.Expected))
	}
	if w.entry.Remote {
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		var alert tls.AlertError
		var record tls.RecordHeaderError
		switch {
		case errors.As(err, &unknown):
			return c.Err(503, "backend_unavailable", "Worker "+w.ID+" certificate does not chain to the configured ca_file")
		case errors.As(err, &hostname):
			return c.Err(503, "backend_unavailable", "Worker "+w.ID+" certificate does not name the configured URL's host")
		case errors.As(err, &invalid):
			return c.Err(503, "backend_unavailable", "Worker "+w.ID+" certificate is not valid: "+invalid.Error())
		case errors.As(err, &alert):
			return c.Err(503, "backend_unavailable", "Worker "+w.ID+" refused the TLS handshake ("+alert.Error()+"); check the client certificate")
		case errors.As(err, &record):
			return c.Err(503, "backend_unavailable", "Worker "+w.ID+" did not answer with TLS; check that it runs in network mode")
		}
	}
	return c.Err(503, "backend_unavailable", "Worker is unavailable; check its process and configuration")
}

func (w *Client) expectedSource() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pinned != "" {
		return "pinned_sha256"
	}
	return "fingerprint recorded on first contact,"
}

// JSON performs one request and decodes the JSON response.
func (w *Client) JSON(path, method string, body any, contentType string) (any, error) {
	if method == "" {
		method = "GET"
	}
	res, err := w.Response(path, method, body, contentType)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, c.Err(503, "backend_unavailable", "Worker is unavailable; check its process and configuration")
	}
	result, err := jsonv.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("worker returned invalid JSON")
	}
	if hook, _ := w.hooks(); hook != nil {
		return hook(path, method, body, result)
	}
	return result, nil
}

// Obj is JSON for an object response; a non-object response decodes as an empty object.
func (w *Client) Obj(path, method string, body any) (*jsonv.Object, error) {
	v, err := w.JSON(path, method, body, "")
	if err != nil {
		return nil, err
	}
	if o, ok := v.(*jsonv.Object); ok {
		return o, nil
	}
	return jsonv.NewObject(), nil
}

// Probe reads info and capabilities concurrently and checks they describe one session.
func (w *Client) Probe() (*Probe, error) {
	var info, caps *jsonv.Object
	var infoErr, capsErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		started := time.Now()
		info, infoErr = w.Obj("/v1/info", "GET", nil)
		if infoErr == nil {
			w.mu.Lock()
			w.latency = time.Since(started)
			w.mu.Unlock()
		}
	}()
	go func() { defer wg.Done(); caps, capsErr = w.Obj("/v1/capabilities", "GET", nil) }()
	wg.Wait()
	if infoErr != nil {
		return nil, infoErr
	}
	if capsErr != nil {
		return nil, capsErr
	}
	disc := w.DiscoveredSession()
	if !jsonEqual(info.Get("session_id"), caps.Get("session_id")) || (disc != "" && disc != info.Get("session_id")) {
		return nil, c.Err(503, "protocol_mismatch", "Worker identity changed between discovery and readiness")
	}
	// Negotiate the highest minor both sides implement; the worker's list is authoritative.
	protocol, err := c.Negotiate(info.Get("supported_protocols"), info.Get("protocol"))
	if err != nil {
		return nil, err
	}
	other, err := c.Negotiate(caps.Get("supported_protocols"), caps.Get("protocol"))
	if err != nil {
		return nil, err
	}
	if other != protocol {
		return nil, c.Err(503, "protocol_mismatch", "Worker info and capabilities disagree about the protocol")
	}
	if err := w.checkCertificate(info); err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.Protocol = protocol
	w.mu.Unlock()
	expected := w.entry.BackendID
	backend := caps.Str("backend_id")
	if !c.Contains(c.BACKENDS, backend) || (expected != "" && backend != expected) {
		message := "Supported backends are " + strings.Join(c.BACKENDS, ", ")
		if expected != "" {
			message = "Configured backend " + expected + " is not the running worker's backend"
		}
		return nil, c.Err(503, "unsupported_backend", message)
	}
	// The configured device is a statement of intent; the worker's report is what is recorded.
	if device := w.entry.Device; device != "" && caps.Str("device") != device {
		return nil, c.Err(503, "backend_unavailable", "Configured device "+device+" is not worker "+w.ID+"'s device")
	}
	p := &Probe{Info: info, Caps: caps, Protocol: protocol}
	if _, hook := w.hooks(); hook != nil {
		return hook(p)
	}
	return p, nil
}

// checkCertificate compares the fingerprint a remote worker reports in /v1/info with the
// certificate it presented, then records it: the configured pin, or on first contact the
// fingerprint seen, which every later connection is held to.
func (w *Client) checkCertificate(info *jsonv.Object) error {
	if !w.entry.Remote {
		return nil
	}
	reported, _ := info.Get("certificate_sha256").(string)
	if info.Str("scheme") != "https" || reported == "" {
		return c.Err(503, "protocol_mismatch", "Remote worker "+w.ID+" does not report its certificate_sha256; network mode needs Engine 0.5.0 or later")
	}
	w.mu.Lock()
	presented, trust, record, recorded := w.presented, w.trust, w.expectedLocked(), w.recorded
	w.mu.Unlock()
	if !strings.EqualFold(reported, presented) {
		return c.Err(503, "certificate_mismatch", "Worker "+w.ID+" reports certificate sha256 "+reported+" but presented "+presented)
	}
	if record == "" {
		record = presented
	}
	if trust != nil && recorded != record {
		if err := trust.RecordCertificateFingerprint(w.ID, w.url, record); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.recorded = record
	w.mu.Unlock()
	return nil
}

func jsonEqual(a, b any) bool { return string(jsonv.MustMarshal(a)) == string(jsonv.MustMarshal(b)) }

// Stop terminates an owned child: SIGTERM, then SIGKILL after six seconds. A remote worker is
// never owned and never signalled.
func (w *Client) Stop() {
	if w.entry.Remote {
		return
	}
	child := w.Child()
	if child == nil || child.Exited() {
		return
	}
	child.Kill(syscall.SIGTERM)
	select {
	case <-child.exited:
	case <-time.After(6 * time.Second):
		child.Kill(syscall.SIGKILL)
		<-child.exited
	}
	_ = child.stdin.Close()
	w.ClearChild()
}
