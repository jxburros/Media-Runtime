package runtime

import (
	"context"
	"errors"
	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/operations"
	"github.com/jxburros/media-runtime/internal/pool"
	"github.com/jxburros/media-runtime/internal/security"
	"github.com/jxburros/media-runtime/internal/store"
	"github.com/jxburros/media-runtime/internal/worker"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Runtime struct {
	Webhooks      *operations.Webhooks
	webhookCancel context.CancelFunc
	Folders       *operations.Folders
	Config        *config.Config
	Store         *store.Store
	Security      *security.Manager
	// Pool is every configured worker. Worker is the first of them, the only one of a legacy
	// single-worker configuration.
	Pool   *pool.Pool
	Worker *worker.Client

	// mu serializes every read-modify-write of a job between the worker loops and API handlers,
	// which the single-threaded TypeScript service got from its event loop. Dispatch decisions
	// are taken under it, so two workers never take the same job.
	mu sync.Mutex
	// lanes run at most one scheduler step at a time for each worker.
	lanes     map[string]*sync.Mutex
	stopping  atomic.Bool
	quit      chan struct{}
	loop      sync.WaitGroup
	idleSince sync.Map
}

func New(cfg *config.Config) (*Runtime, error) {
	s, err := store.Open(cfg.Directory)
	if err != nil {
		return nil, err
	}
	sec, err := security.New(s.DB, cfg.Directory)
	if err != nil {
		s.Close()
		return nil, err
	}
	webhooks, err := operations.OpenWebhooks(cfg.Directory, cfg.Webhooks)
	if err != nil {
		s.Close()
		return nil, err
	}
	folders, err := operations.OpenFolders(cfg.Directory)
	if err != nil {
		s.Close()
		return nil, err
	}
	entries, err := folders.Scan()
	if err != nil {
		s.Close()
		return nil, err
	}
	cfg.AddArtifacts(entries)
	p := pool.New(cfg)
	lanes := map[string]*sync.Mutex{}
	for _, m := range p.Members {
		lanes[m.ID] = &sync.Mutex{}
		// A remote worker's recorded certificate fingerprint lives in the state directory.
		m.Client.SetTrust(s)
	}
	return &Runtime{Webhooks: webhooks, Folders: folders, Config: cfg, Store: s, Security: sec, Pool: p, Worker: p.Members[0].Client, lanes: lanes, quit: make(chan struct{})}, nil
}

// Start launches or checks every worker, then runs one scheduler loop per worker, each every
// poll interval.
func (r *Runtime) Start() error {
	if err := r.Pool.Start(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.Config.Directory, "inputs"), 0o700); err != nil {
		return err
	}
	r.unconfigured()
	r.startOperations()
	for _, m := range r.Pool.Members {
		r.loop.Add(1)
		go func(m *pool.Member) {
			defer r.loop.Done()
			ticker := time.NewTicker(time.Duration(r.Config.PollIntervalMS) * time.Millisecond)
			defer ticker.Stop()
			r.Tick(m)
			for {
				select {
				case <-r.quit:
					return
				case <-ticker.C:
					r.Tick(m)
				}
			}
		}(m)
	}
	return nil
}

// unconfigured fails running jobs whose worker is no longer in the configuration: nothing can
// reconcile their attempt.
func (r *Runtime) unconfigured() {
	for _, job := range r.Store.All("running") {
		if r.Pool.Get(job.Str("worker_id")) == nil {
			r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Worker "+job.Str("worker_id")+" is no longer configured; this job was not regenerated"))
		}
	}
}

// Stop waits for every worker's current step, stops owned workers and closes the store.
func (r *Runtime) Stop() {
	if r.stopping.Swap(true) {
		return
	}
	close(r.quit)
	if r.webhookCancel != nil {
		r.webhookCancel()
	}
	r.loop.Wait()
	for _, lane := range r.lanes {
		lane.Lock()
		lane.Unlock()
	}
	r.Pool.Stop()
	r.Store.Close()
}

func (r *Runtime) Owned(jobID, namespace string) (*jsonv.Object, error) {
	job := r.Store.Get(jobID, namespace)
	if job == nil {
		return nil, c.Err(404, "not_found", "Job not found")
	}
	return job, nil
}

func (r *Runtime) InputPath(inputID string) string {
	return filepath.Join(r.Config.Directory, "inputs", inputID+".png")
}

func (r *Runtime) expiry() any {
	days := r.Config.OutputRetentionDays
	if days == nil || *days == 0 {
		return nil
	}
	return c.ISO(time.Now().Add(time.Duration(*days*86400000) * time.Millisecond))
}

// StoreInput keeps a durable, namespace-owned PNG input; the worker only ever receives bytes
// the runtime verified.
func (r *Runtime) StoreInput(bytes []byte, namespace string) (*jsonv.Object, error) {
	if len(bytes) > r.Config.MaxInputBytes {
		return nil, c.Err(413, "input_too_large", "Input exceeds max_input_bytes")
	}
	width, height, err := c.PNGDimensions(bytes)
	if err != nil {
		return nil, err
	}
	record := jsonv.O(
		"input_id", c.NewInputID(),
		"namespace", namespace,
		"mime_type", "image/png",
		"size", float64(len(bytes)),
		"sha256", c.SHA256Hex(bytes),
		"width", float64(width),
		"height", float64(height),
		"created_at", c.Now(),
		"expires_at", r.expiry(),
	)
	if err := atomicFile(r.InputPath(record.Str("input_id")), bytes); err != nil {
		return nil, err
	}
	r.Store.InsertInput(record)
	return record, nil
}

func (r *Runtime) OwnedInput(inputID, namespace string) (*jsonv.Object, error) {
	record := r.Store.Input(inputID, namespace)
	if record == nil {
		return nil, c.Err(404, "not_found", "Input not found")
	}
	return record, nil
}

func (r *Runtime) referenced(inputID string) bool {
	for _, j := range r.Store.All("") {
		if c.Terminal(j.Str("state")) {
			continue
		}
		if j.Obj("input").Str("input_id") == inputID || j.Obj("mask").Str("input_id") == inputID || j.Obj("overlay").Str("input_id") == inputID || j.Obj("control_image").Str("input_id") == inputID || j.Obj("reference_image").Str("input_id") == inputID {
			return true
		}
		for _, op := range Operations(j) {
			if op.Obj("input").Str("input_id") == inputID || op.Obj("mask").Str("input_id") == inputID || op.Obj("overlay").Str("input_id") == inputID || op.Obj("control_image").Str("input_id") == inputID || op.Obj("reference_image").Str("input_id") == inputID {
				return true
			}
		}
	}
	return false
}

func (r *Runtime) DeleteInput(inputID, namespace string) error {
	if _, err := r.OwnedInput(inputID, namespace); err != nil {
		return err
	}
	if r.referenced(inputID) {
		return c.Err(409, "input_in_use", "An active job references this input")
	}
	if err := os.Remove(r.InputPath(inputID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r.Store.DeleteInput(inputID)
	return nil
}

// nullish is JavaScript's `a ?? b` over decoded JSON values.
func nullish(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func strList(v any) []string {
	var out []string
	for _, e := range asArray(v) {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asArray(v any) []any { a, _ := v.([]any); return a }

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	}
	return true
}

// Caller is the authenticated identity a request runs under.
type Caller struct {
	KeyID            string
	Models           []string
	ConcurrencyLimit int
	QueueLimit       int
	Namespace        string
	AllowFallbacks   bool
}

func forbidden(field, message string) error {
	return c.Err(400, "invalid_request", message, field)
}
