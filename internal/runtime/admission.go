package runtime

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/store"
	"time"
)

func (r *Runtime) Submit(body any, caller Caller, key *string) (*jsonv.Object, error) {
	if err := caller.CheckModels(body); err != nil {
		return nil, err
	}
	namespace := caller.Namespace
	parsed, err := c.Validate(c.Submission, body)
	if err != nil {
		return nil, err
	}
	digest := c.Hash(parsed)
	if key != nil && (len(*key) == 0 || utf16Len(*key) > 200) {
		return nil, c.Err(400, "invalid_idempotency_key", "Idempotency-Key must contain 1–200 characters")
	}
	existing := func(s *store.Store) (*jsonv.Object, error) {
		if key == nil {
			return nil, nil
		}
		stored, jobID, ok := s.Idempotency(namespace, *key)
		if !ok {
			return nil, nil
		}
		if stored != digest {
			return nil, c.Err(409, "idempotency_conflict", "Idempotency key was already used for a different request")
		}
		return s.Get(jobID, ""), nil
	}
	if retry, err := existing(r.Store); err != nil || retry != nil {
		return retry, err
	}
	resolved, err := r.Resolve(parsed, caller)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var job *jsonv.Object
	err = r.Store.Transaction(func(tx *store.Store) error {
		raced, err := existing(tx)
		if err != nil || raced != nil {
			job = raced
			return err
		}
		if len(tx.All("queued")) >= r.Config.QueueCapacity {
			return c.Err(429, "queue_full", "Queue is full; retry later")
		}
		if caller.KeyID != "" && caller.QueueLimit > 0 {
			count := 0
			for _, pending := range tx.All("queued") {
				if pending.Str("key_id") == caller.KeyID {
					count++
				}
			}
			if count >= caller.QueueLimit {
				return c.Err(429, "key_queue_full", "This key has reached its queue share")
			}
		}
		now := c.Now()
		job = jsonv.O(
			"job_id", "job_"+c.UUID(),
			"namespace", namespace,
			"state", "queued",
			"stage", nil,
			"cancel_requested", false,
			"created_at", now,
			"updated_at", now,
		).Assign(resolved).
			Set("key_id", jsonv.Maybe(caller.KeyID)).
			Set("key_concurrency_limit", float64(caller.ConcurrencyLimit)).
			Set("worker_sequence", 0.0).
			Set("outputs", []any{}).
			Set("current_operation", 0.0).
			Set("worker_id", nil).
			Set("retry_of", jsonv.Maybe(parsed.Get("retry_of")))
		tx.Insert(job)
		tx.Event(job, "job_state", jsonv.O("state", "queued"))
		if key != nil {
			tx.InsertIdempotency(namespace, *key, digest, job.Str("job_id"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Cancel records cancellation intent; a queued job is cancelled immediately.
func (r *Runtime) Cancel(jobID, namespace string) (*jsonv.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := r.Owned(jobID, namespace)
	if err != nil {
		return nil, err
	}
	if c.Terminal(job.Str("state")) {
		return job, nil
	}
	job.Set("cancel_requested", true)
	if job.Get("cancel_at") == nil {
		job.Set("cancel_at", float64(time.Now().UnixMilli()))
	}
	err = r.Store.Transaction(func(tx *store.Store) error {
		tx.Save(job)
		tx.Event(job, "job_state", jsonv.O("state", job.Str("state"), "cancel_requested", true))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if job.Str("state") == "queued" {
		r.Store.Finish(job, "cancelled", nil)
	}
	return r.Store.Get(jobID, ""), nil
}

// refresh reloads cancellation intent, the only job field API handlers change while it runs.
func (r *Runtime) refresh(job *jsonv.Object) {
	latest := r.Store.Get(job.Str("job_id"), "")
	if latest == nil {
		return
	}
	job.Set("cancel_requested", latest.Get("cancel_requested"))
	job.Set("cancel_at", jsonv.Maybe(latest.Get("cancel_at")))
}

// save writes the scheduler's copy of a job (plus any events) without losing a cancellation.
func (r *Runtime) save(job *jsonv.Object, events func(tx *store.Store) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(job)
	mirror(job)
	return r.Store.Transaction(func(tx *store.Store) error {
		if events != nil {
			if err := events(tx); err != nil {
				return err
			}
		}
		tx.Save(job)
		return nil
	})
}

// finish settles a job and its current operation; a linear job's error names the operation.
func (r *Runtime) finish(job *jsonv.Object, state string, errorRecord *jsonv.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(job)
	if op, _ := current(job); op != nil && !c.Terminal(op.Str("state")) {
		op.Set("state", state)
		if errorRecord != nil {
			op.Set("error", errorRecord.Clone())
			if c.IsLinear(job.Obj("recipe")) {
				errorRecord.Set("operation", op.Get("op_id"))
			}
		}
	}
	mirror(job)
	r.Store.Finish(job, state, errorRecord)
}

// Operations returns a job's operation records; they are shared with the job, so changes to
// them are saved with it.
func Operations(job *jsonv.Object) []*jsonv.Object {
	var out []*jsonv.Object
	for _, raw := range job.Arr("operations") {
		if op, ok := raw.(*jsonv.Object); ok {
			out = append(out, op)
		}
	}
	return out
}

// current is the operation the scheduler is driving and its index.
func current(job *jsonv.Object) (*jsonv.Object, int) {
	ops := Operations(job)
	index, _ := job.Num("current_operation")
	if int(index) < 0 || int(index) >= len(ops) {
		return nil, 0
	}
	return ops[int(index)], int(index)
}

// mirrored are the current operation's worker identities, also kept at the top level of the
// job where single-operation jobs have always recorded them.
var mirrored = []string{"attempt_id", "session_id", "execution_id", "worker_input_id", "load_operation_id", "dispatch_phase", "worker_sequence", "applied_fallbacks", "released"}

func mirror(job *jsonv.Object) {
	op, index := current(job)
	if op == nil {
		return
	}
	for _, k := range mirrored {
		if op.Has(k) {
			job.Set(k, op.Get(k))
		} else {
			job.Delete(k)
		}
	}
	if index == 0 {
		job.Set("backend", op.Get("backend"))
	}
}

// annotate adds a linear job's operation to the data of a stage or progress event; schema-1
// jobs keep the worker's data unchanged.
func annotate(job, op *jsonv.Object, index int, data any) any {
	o, ok := data.(*jsonv.Object)
	if !ok || !c.IsLinear(job.Obj("recipe")) {
		return data
	}
	return o.Clone().Set("op_id", op.Get("op_id")).Set("operation_index", float64(index)).Set("operation_count", float64(len(Operations(job))))
}

// runningOn is the job running on a worker; each worker runs at most one.
func (r *Runtime) runningOn(workerID string) *jsonv.Object {
	for _, job := range r.Store.All("running") {
		if job.Str("worker_id") == workerID {
			return job
		}
	}
	return nil
}

// Tick advances one worker's scheduler by one step; overlapping calls return immediately.
