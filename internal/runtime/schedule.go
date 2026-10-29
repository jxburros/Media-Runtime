package runtime

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
	"github.com/jxburros/media-runtime/internal/worker"
)

// Lane is one worker as the scheduler sees it: its last successful probe (nil when it is
// unavailable) and whether it has no running job.
type Lane struct {
	ID    string
	Probe *worker.Probe
	Idle  bool
}

// Assignment is one dispatch the scheduler would make now.
type Assignment struct {
	Job      *jsonv.Object
	WorkerID string
}

// Assign plans the dispatches of the queued jobs (in admission order) onto the idle lanes (in
// configuration order). Each job goes to an eligible idle worker whose loaded identity is the
// job's when one exists, else to the first eligible idle worker; ties go to configuration order.
// FIFO is literal: when a job has no eligible idle worker, no later job is dispatched ahead of
// it unless reorder is set, in which case later jobs may use the workers it cannot.
func Assign(queued []*jsonv.Object, lanes []Lane, reorder bool) []Assignment {
	out := []Assignment{}
	taken := map[string]bool{}
	for _, job := range queued {
		match, any := "", ""
		for _, lane := range lanes {
			if taken[lane.ID] || !lane.Idle || !Eligible(job, lane) {
				continue
			}
			if any == "" {
				any = lane.ID
			}
			if op, _ := current(job); match == "" && op != nil && loadMatches(lane.Probe.Info, job, op) {
				match = lane.ID
			}
		}
		chosen := match
		if chosen == "" {
			chosen = any
		}
		if chosen == "" {
			if reorder {
				continue
			}
			break
		}
		taken[chosen] = true
		out = append(out, Assignment{Job: job, WorkerID: chosen})
	}
	return out
}

// Eligible reports whether a ready worker may run a queued job: it is the pinned worker, or one
// of the candidates the job was admitted against still negotiating at least the minor it did
// then, and it still advertises every operation's task, family and backend.
func Eligible(job *jsonv.Object, lane Lane) bool {
	if lane.Probe == nil {
		return false
	}
	info, caps, protocol := lane.Probe.Info, lane.Probe.Caps, lane.Probe.Protocol
	minor := c.Minor(protocol)
	if info.Str("state") != "ready" || !(truthy(caps.Get("ready")) || cpuFallbackReady(caps, protocol, job.Obj("request"))) {
		return false
	}
	if pin := job.Obj("request").Str("worker_id"); pin != "" && pin != lane.ID {
		return false
	}
	if job.Has("candidates") {
		listed := false
		for _, raw := range job.Arr("candidates") {
			if candidate, ok := raw.(*jsonv.Object); ok && candidate.Str("worker_id") == lane.ID && c.Minor(candidate.Str("protocol")) <= minor {
				listed = true
			}
		}
		if !listed {
			return false
		}
	}
	tasks := []string{"text-to-image"}
	if _, ok := caps.Get("tasks").([]any); ok {
		tasks = strList(caps.Get("tasks"))
	}
	for _, op := range Operations(job) {
		task := op.Str("task")
		if !c.Contains(tasks, task) || minor < c.TaskMinor(task) || op.Obj("backend").Str("backend_id") != caps.Str("backend_id") {
			return false
		}
		if family := op.Obj("model").Obj("bundle").Str("family"); minor >= 2 && family != "" && !loads(caps, family) {
			return false
		}
	}
	return true
}

// loadMatches reports whether a worker's loaded bundle is an operation's load identity: the
// bundle, the job's load options and the adapter set with its scales.
func loadMatches(info, job, op *jsonv.Object) bool {
	desired := job.Obj("request").Obj("backend_options").Get(op.Obj("backend").Str("backend_id"))
	identity := []any{}
	for _, raw := range asArray(op.Get("adapters")) {
		a := raw.(*jsonv.Object)
		adapter := a.Obj("adapter")
		identity = append(identity, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "scale", a.Get("scale")))
	}
	loaded := info.Obj("loaded_bundle")
	loadedIdentity := []any{}
	for _, raw := range asArray(loaded.Get("adapters")) {
		a, _ := raw.(*jsonv.Object)
		loadedIdentity = append(loadedIdentity, jsonv.O("adapter_id", jsonv.Maybe(a.Get("adapter_id")), "digest", jsonv.Maybe(a.Get("digest")), "scale", jsonv.Maybe(a.Get("scale"))))
	}
	return loaded != nil && jsonEqual(loaded.Get("bundle_digest"), op.Obj("model").Obj("bundle").Get("bundle_digest")) &&
		c.Hash(loaded.Get("options")) == c.Hash(desired) && c.Hash(loadedIdentity) == c.Hash(identity)
}

// next is the queued job this worker should dispatch now, or nil; the caller holds r.mu. Jobs
// that no configured worker can ever run (their pinned worker or every candidate was removed
// from the configuration) are failed first so they cannot hold the queue.
func (r *Runtime) next(m *pool.Member) *jsonv.Object {
	queued := []*jsonv.Object{}
	for _, job := range r.Store.All("queued") {
		if r.orphaned(job) {
			r.Store.Finish(job, "failed", jsonv.O("code", "backend_unavailable", "message", "No configured worker can run this job"))
			continue
		}
		queued = append(queued, job)
	}
	if len(queued) == 0 {
		return nil
	}
	busy := map[string]bool{}
	for _, job := range r.Store.All("running") {
		busy[job.Str("worker_id")] = true
	}
	lanes := []Lane{}
	for _, view := range r.Pool.Views() {
		lane := Lane{ID: view.Member.ID, Idle: !busy[view.Member.ID]}
		if view.Ready() {
			lane.Probe = view.Probe
		}
		lanes = append(lanes, lane)
	}
	for _, a := range Assign(queued, lanes, r.Config.AllowReordering) {
		if a.WorkerID == m.ID {
			return a.Job
		}
	}
	return nil
}

// orphaned is a queued job whose pinned worker, or every candidate worker, is not configured.
func (r *Runtime) orphaned(job *jsonv.Object) bool {
	if pin := job.Obj("request").Str("worker_id"); pin != "" {
		return r.Pool.Get(pin) == nil
	}
	if !job.Has("candidates") {
		return false
	}
	for _, raw := range job.Arr("candidates") {
		if candidate, ok := raw.(*jsonv.Object); ok && r.Pool.Get(candidate.Str("worker_id")) != nil {
			return false
		}
	}
	return true
}

// attribute records the worker a job is dispatched to: its ID and device identity on the job,
// and on every operation (a linear job runs all its operations on this worker) with the worker
// facts its backend record carries, which the resolving worker supplied at admission.
func attribute(job *jsonv.Object, workerID string, p *worker.Probe) {
	info, caps := p.Info, p.Caps
	var identity any
	if c.Minor(p.Protocol) >= 2 {
		identity = info.Get("device_identity")
	}
	job.Set("worker_id", workerID)
	job.Set("device_identity", identity)
	for _, op := range Operations(job) {
		op.Set("worker_id", workerID)
		backend := op.Obj("backend")
		if backend == nil {
			continue
		}
		backend.Set("ownership", nullish(caps.Get("ownership"), "reference"))
		backend.Set("protocol", p.Protocol)
		backend.Set("device", jsonv.Maybe(caps.Get("device")))
		backend.Set("memory_budget_mb", jsonv.Maybe(caps.Get("memory_budget_mb")))
		if identity != nil {
			backend.Set("device_identity", identity)
		}
	}
}
