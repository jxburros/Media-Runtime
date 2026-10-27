// Package worker is the Media Engine client: HTTP calls with the worker credential, protocol
// negotiation, and supervision of an owned child process.
package worker

import (
	"bufio"
	"bytes"
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
)

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
	cfg  *config.Config
	mu   sync.Mutex
	url  string
	tok  string
	disc string
	// Protocol is the minor negotiated with the current worker session; set by Probe.
	Protocol string
	child    *Child
	http     *http.Client

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

func New(cfg *config.Config) *Client {
	timeout := time.Duration(cfg.WorkerTimeoutMS) * time.Millisecond
	return &Client{
		cfg:      cfg,
		url:      strings.TrimSuffix(cfg.Worker.URL, "/"),
		tok:      os.Getenv(cfg.Worker.TokenEnv),
		Protocol: c.PROTOCOLS[0],
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are refused") },
			Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
		},
	}
}

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

// Start launches the configured worker command, or checks the external worker credential.
func (w *Client) Start() error {
	command := w.cfg.Worker.Command
	if len(command) == 0 {
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
		return nil, c.Err(503, "backend_unavailable", "Worker is unavailable; check its process and configuration")
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
	go func() { defer wg.Done(); info, infoErr = w.Obj("/v1/info", "GET", nil) }()
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
	w.mu.Lock()
	w.Protocol = protocol
	w.mu.Unlock()
	expected := w.cfg.Worker.BackendID
	backend := caps.Str("backend_id")
	if !c.Contains(c.BACKENDS, backend) || (expected != "" && backend != expected) {
		message := "Supported backends are " + strings.Join(c.BACKENDS, ", ")
		if expected != "" {
			message = "Configured backend " + expected + " is not the running worker's backend"
		}
		return nil, c.Err(503, "unsupported_backend", message)
	}
	p := &Probe{Info: info, Caps: caps, Protocol: protocol}
	if _, hook := w.hooks(); hook != nil {
		return hook(p)
	}
	return p, nil
}

func jsonEqual(a, b any) bool { return string(jsonv.MustMarshal(a)) == string(jsonv.MustMarshal(b)) }

// Stop terminates an owned child: SIGTERM, then SIGKILL after six seconds.
func (w *Client) Stop() {
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
