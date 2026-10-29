// Package pool holds the configured workers: one client per entry, each probed on its own, the
// last view of each (info, capabilities and negotiated minor) that the scheduler and the API
// read, and the rule that keeps one worker per device.
package pool

import (
	"errors"
	"fmt"
	"sync"

	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
)

// Member is one configured worker and its client.
type Member struct {
	ID     string
	Config config.Worker
	Client *worker.Client
}

// View is the last probe of a member: the probe, or the error that replaced it, and the worker
// reporting the same device identity when there is one.
type View struct {
	Member   *Member
	Probe    *worker.Probe
	Err      error
	Conflict string
}

// Ready is a view whose last probe succeeded without a device conflict.
func (v View) Ready() bool { return v.Probe != nil && v.Err == nil && v.Conflict == "" }

type state struct {
	probe    *worker.Probe
	err      error
	key      string
	conflict string
}

// Pool is the configured workers in configuration order.
type Pool struct {
	Members []*Member
	strict  bool
	mu      sync.Mutex
	states  map[string]*state
}

func New(cfg *config.Config) *Pool {
	p := &Pool{strict: cfg.StrictDevices, states: map[string]*state{}}
	for _, entry := range cfg.Pool() {
		p.Members = append(p.Members, &Member{ID: entry.WorkerID, Config: entry, Client: worker.New(cfg, entry)})
		p.states[entry.WorkerID] = &state{err: c.Err(503, "backend_unavailable", "Worker has not been probed")}
	}
	return p
}

// Get is the member with the worker ID, or nil.
func (p *Pool) Get(id string) *Member {
	for _, m := range p.Members {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// Start launches every owned worker (or checks each external credential), probes them all and
// applies the device rule: with strict_devices two workers reporting one device identity refuse
// the start; otherwise both stay configured but unavailable.
func (p *Pool) Start() error {
	for _, m := range p.Members {
		if err := m.Client.Start(); err != nil {
			p.Stop()
			if len(p.Members) > 1 {
				return fmt.Errorf("Worker %s: %w", m.ID, err)
			}
			return err
		}
	}
	p.ProbeAll()
	if !p.strict {
		return nil
	}
	p.mu.Lock()
	var duplicate error
	for _, m := range p.Members {
		if s := p.states[m.ID]; s.conflict != "" {
			duplicate = fmt.Errorf("Workers %s and %s report the same device identity; give each worker its own device or set strict_devices to false", m.ID, s.conflict)
			break
		}
	}
	p.mu.Unlock()
	if duplicate != nil {
		p.Stop()
	}
	return duplicate
}

// Stop terminates every owned worker.
func (p *Pool) Stop() {
	var wg sync.WaitGroup
	for _, m := range p.Members {
		wg.Add(1)
		go func(m *Member) { defer wg.Done(); m.Client.Stop() }(m)
	}
	wg.Wait()
}

// Probe probes one member and records the result. A member that reports another member's
// device identity is unavailable, and so is that other member, until one of them changes.
func (p *Pool) Probe(m *Member) (*worker.Probe, error) {
	probe, err := m.Client.Probe()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.states[m.ID]
	if err != nil {
		s.probe, s.err, s.key = nil, err, ""
	} else {
		s.probe, s.err, s.key = probe, nil, IdentityKey(probe)
	}
	p.recheck()
	if err != nil {
		return nil, err
	}
	if s.conflict != "" {
		return nil, conflictError(m.ID, s.conflict)
	}
	return probe, nil
}

func conflictError(a, b string) error {
	return c.Err(503, "backend_unavailable", fmt.Sprintf("Workers %s and %s report the same device identity", a, b))
}

// recheck recomputes every member's conflict from the last known identities.
func (p *Pool) recheck() {
	for _, m := range p.Members {
		s := p.states[m.ID]
		s.conflict = ""
		if s.key == "" {
			continue
		}
		for _, other := range p.Members {
			if other.ID != m.ID && p.states[other.ID].key == s.key {
				s.conflict = other.ID
				break
			}
		}
	}
}

// IdentityKey is the part of a 1.2 worker's device identity that names the device: host, device
// and index (the process ID is not part of it). Workers before 1.2 report none and are not
// compared.
func IdentityKey(probe *worker.Probe) string {
	id := probe.Info.Obj("device_identity")
	if id == nil || c.Minor(probe.Protocol) < 2 {
		return ""
	}
	return string(jsonv.MustMarshal([]any{id.Get("host_id"), id.Get("device"), id.Get("index")}))
}

// ProbeAll probes every member concurrently and returns the views in configuration order.
func (p *Pool) ProbeAll() []View {
	var wg sync.WaitGroup
	for _, m := range p.Members {
		wg.Add(1)
		go func(m *Member) { defer wg.Done(); _, _ = p.Probe(m) }(m)
	}
	wg.Wait()
	return p.Views()
}

// Views is the last probe of every member in configuration order.
func (p *Pool) Views() []View {
	p.mu.Lock()
	defer p.mu.Unlock()
	views := make([]View, len(p.Members))
	for i, m := range p.Members {
		s := p.states[m.ID]
		views[i] = View{Member: m, Probe: s.probe, Err: s.err, Conflict: s.conflict}
		if s.conflict != "" {
			views[i].Err = conflictError(m.ID, s.conflict)
		}
	}
	return views
}

// First probes the members in configuration order and returns the first available one; with
// none it returns the first member's error. Registration and verification use it.
func (p *Pool) First() (*Member, *worker.Probe, error) {
	var first error
	for _, view := range p.ProbeAll() {
		if view.Ready() {
			return view.Member, view.Probe, nil
		}
		if first == nil {
			first = view.Err
		}
	}
	if first == nil {
		first = errors.New("no worker is configured")
	}
	return nil, nil, first
}
