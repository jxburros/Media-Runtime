package runtime

import (
	"errors"
	"fmt"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
	"github.com/jxburros/media-runtime/internal/store"
	"net/url"
	"os"
	"time"
)

func (r *Runtime) Tick(m *pool.Member) {
	lane := r.lanes[m.ID]
	if !lane.TryLock() {
		return
	}
	defer lane.Unlock()
	if r.stopping.Load() {
		return
	}
	err := r.tick(m)
	if err == nil {
		r.maybeIdleUnload(m)
	}
	var api *c.ApiError
	if err == nil || !errors.As(err, &api) {
		return
	}
	// Transient transport errors preserve dispatch identity for reconciliation; never regenerate.
	// A refused certificate is one: nothing was said to the worker, and the job resumes from its
	// snapshot once the operator restores the expected certificate or pin.
	if job := r.runningOn(m.ID); job != nil && !transient(api) {
		code, message := api.Code, api.Message
		if api.Status == 404 {
			code, message = "execution_interrupted", "Worker record is no longer available"
		}
		r.finish(job, "failed", jsonv.O("code", code, "message", message))
	}
}

// transient is a transport failure that leaves the job's dispatch identity for reconciliation:
// an unreachable worker, or a remote worker whose certificate was refused before anything was
// said to it.
func transient(api *c.ApiError) bool {
	return api.Code == "backend_unavailable" || api.Code == "certificate_mismatch"
}

func (r *Runtime) tick(m *pool.Member) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("scheduler: %v", p)
		}
	}()
	client := m.Client
	if child := client.Child(); child != nil && child.Exited() {
		// A lost worker interrupts only its own job.
		if interrupted := r.runningOn(m.ID); interrupted != nil {
			r.finish(interrupted, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker exited; this job was not regenerated"))
		}
		client.ClearChild()
		if err := client.Start(); err != nil {
			return err
		}
	}
	p, err := r.Pool.Probe(m)
	if err != nil {
		return err
	}
	info, caps, protocol := p.Info, p.Caps, p.Protocol
	session := info.Get("session_id")
	// Release an operation's worker records only after its durable terminal commit: every
	// operation of a finished job, and each committed operation of a running linear job. Retry
	// collection acknowledgements after disconnects. Session IDs are per worker process, so each
	// worker releases only what it holds.
	for _, done := range r.Store.All("") {
		changed := false
		for _, op := range Operations(done) {
			if !c.Terminal(op.Str("state")) || truthy(op.Get("released")) ||
				!(truthy(op.Get("execution_id")) || truthy(op.Get("worker_input_id")) || truthy(op.Get("worker_mask_id")) || truthy(op.Get("worker_overlay_id")) || truthy(op.Get("worker_control_id")) || truthy(op.Get("worker_reference_id"))) || !jsonEqual(op.Get("session_id"), session) {
				continue
			}
			err := func() error {
				if id := op.Str("execution_id"); id != "" {
					if _, err := client.JSON("/v1/executions/"+id, "DELETE", nil, ""); err != nil {
						return err
					}
				}
				for _, field := range []string{"worker_input_id", "worker_mask_id", "worker_overlay_id", "worker_control_id", "worker_reference_id"} {
					if id := op.Str(field); id != "" {
						if _, err := client.JSON("/v1/inputs/"+id, "DELETE", nil, ""); err != nil {
							var api *c.ApiError
							if !errors.As(err, &api) || api.Status != 404 {
								return err
							}
						}
					}
				}
				return nil
			}()
			var api *c.ApiError
			if err == nil || (errors.As(err, &api) && api.Status == 404) {
				op.Set("released", true)
				changed = true
			}
		}
		if changed {
			if err := r.save(done, nil); err != nil {
				return err
			}
		}
	}
	job := r.runningOn(m.ID)
	if job == nil {
		if info.Str("state") != "ready" {
			return nil
		}
		r.mu.Lock()
		job = r.next(m)
		if job == nil {
			r.mu.Unlock()
			return nil
		}
		err := r.Store.Transaction(func(tx *store.Store) error {
			job.Set("state", "running")
			job.Set("stage", nil)
			attribute(job, m.ID, p)
			op, _ := current(job)
			dispatch(op, caps, session)
			mirror(job)
			tx.Save(job)
			data := jsonv.O("state", "running", "worker_id", m.ID)
			if c.Minor(protocol) >= 2 {
				data.Set("device_identity", jsonv.Maybe(info.Get("device_identity")))
			}
			tx.Event(job, "job_state", data)
			return nil
		})
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	op, index := current(job)
	if op.Str("attempt_id") == "" {
		// The next operation of a linear job: its predecessor's outputs are already committed.
		dispatch(op, caps, session)
		if err := r.save(job, nil); err != nil {
			return err
		}
	}
	if !jsonEqual(op.Get("session_id"), session) {
		r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Worker session ended; this job was not regenerated"))
		return nil
	}
	r.refresh(job)
	grace := time.Duration(r.Config.CancellationGraceMS) * time.Millisecond
	// overdue is a remote worker's cancellation past the deadline. A remote worker is never
	// owned, so nothing is terminated: the job settles only when the worker itself reports a
	// terminal state (or its session changes, handled above).
	overdue := false
	if truthy(job.Get("cancel_requested")) {
		at, ok := job.Num("cancel_at")
		if !ok || at == 0 {
			at = float64(time.Now().UnixMilli())
		}
		late := time.Since(time.UnixMilli(int64(at))) > grace
		switch {
		case late && m.Config.Remote:
			overdue = true
		case late && client.Child() != nil:
			client.Stop()
			r.finish(job, "failed", jsonv.O("code", "execution_interrupted", "message", "Owned worker was forcibly stopped after the cancellation deadline", "forced_termination", true))
			return client.Start()
		}
	}
	backend := op.Obj("backend")
	if op.Str("dispatch_phase") == "loading" {
		if opID := op.Str("load_operation_id"); opID != "" {
			operation, err := client.Obj("/v1/operations/"+opID, "GET", nil)
			if err != nil {
				return err
			}
			if operation.Str("state") == "running" {
				return nil
			}
			if _, err := client.JSON("/v1/operations/"+opID, "DELETE", nil, ""); err != nil {
				return err
			}
			op.Delete("load_operation_id")
			if operation.Str("state") == "failed" {
				code := operation.Obj("error").Str("code")
				if code == "" {
					code = "model_load_failed"
				}
				r.finish(job, "failed", jsonv.O("code", code, "message", "Selected model could not be loaded"))
				return nil
			}
			if estimate := operation.Get("memory_estimate"); truthy(estimate) {
				backend.Set("memory_estimate", estimate)
			}
			for _, k := range []string{"resolved_options", "resolutions"} {
				if v := operation.Get(k); v != nil {
					backend.Set(k, v)
				}
			}
			return r.save(job, nil)
		}
		if s := info.Str("state"); s == "loading" || s == "unloading" {
			return nil
		}
		r.refresh(job)
		// Cancellation is honored before every dispatch, including between operations.
		if truthy(job.Get("cancel_requested")) {
			r.finish(job, "cancelled", nil)
			return nil
		}
		desired := job.Obj("request").Obj("backend_options").Get(backend.Str("backend_id"))
		loaded := info.Obj("loaded_bundle")
		model := op.Obj("model")
		if !loadMatches(info, job, op) {
			body := jsonv.O("protocol", protocol, "bundle", model.Get("bundle"), "options", desired)
			if len(op.Arr("controlnets")) > 0 {
				body.Set("controlnets", op.Get("controlnets"))
			}
			if len(op.Arr("conditioning")) > 0 {
				body.Set("conditioning", op.Get("conditioning"))
			}
			if len(asArray(op.Get("adapters"))) > 0 {
				body.Set("adapters", op.Get("adapters"))
			}
			if fallbacks := job.Obj("request").Arr("authorized_fallbacks"); len(fallbacks) > 0 {
				body.Set("authorized_fallbacks", fallbacks)
			}
			load, err := client.Obj("/v1/model/load", "POST", body)
			if err != nil {
				return err
			}
			op.Set("load_operation_id", jsonv.Maybe(load.Get("operation_id")))
			job.Set("stage", "loading")
			job.Delete("progress")
			return r.save(job, func(tx *store.Store) error {
				tx.Event(job, "stage", annotate(job, op, index, jsonv.O("stage", "loading")))
				return nil
			})
		}
		op.Set("dispatch_phase", "executing")
		// Record any fallback the loaded bundle runs on; it was authorized by this or an earlier job.
		var applied []any
		if c.Minor(protocol) >= 2 {
			applied = asArray(nullish(loaded.Get("applied_fallbacks"), []any{}))
			op.Set("applied_fallbacks", applied)
			backend.Set("applied_fallbacks", applied)
			if len(applied) > 0 {
				r.Security.Audit.Record("fallback.applied", job.Str("key_id"), job.Str("job_id"))
			}
		}
		if err := r.save(job, func(tx *store.Store) error {
			if len(applied) > 0 {
				tx.Event(job, "stage", annotate(job, op, index, jsonv.O("stage", "loading", "fallback", applied)))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	var snapshot *jsonv.Object
	if op.Str("execution_id") == "" {
		// Query the same session before resending an ambiguous acceptance. A retry uses the SAME attempt.
		snapshot, err = client.Obj("/v1/executions?attempt_id="+url.QueryEscape(op.Str("attempt_id")), "GET", nil)
		if err != nil {
			var api *c.ApiError
			if !errors.As(err, &api) || api.Status != 404 {
				return err
			}
			r.refresh(job)
			if truthy(job.Get("cancel_requested")) {
				r.finish(job, "cancelled", nil)
				return nil
			}
			if reused, err := r.tryCached(client, job, op, index); reused || err != nil {
				return err
			}
			inputs := op.Obj("inputs").Clone()
			if c.UsesImage(op.Str("task")) {
				if op.Str("worker_input_id") == "" {
					// Hand the worker the verified bytes under a session-scoped handle, never a path.
					bytes, digest, err := r.operationInput(job, op)
					if err != nil {
						return err
					}
					uploaded, err := client.JSON("/v1/inputs", "POST", bytes, "image/png")
					if err != nil {
						return err
					}
					up, _ := uploaded.(*jsonv.Object)
					if up.Str("sha256") != digest {
						return c.Err(503, "protocol_mismatch", "Worker input digest mismatch")
					}
					op.Set("worker_input_id", jsonv.Maybe(up.Get("input_id")))
					if err := r.save(job, nil); err != nil {
						return err
					}
				}
				inputs.Set("image", op.Get("worker_input_id"))
			}
			if mask := op.Obj("mask"); mask != nil {
				if op.Str("worker_mask_id") == "" {
					data, err := os.ReadFile(r.InputPath(mask.Str("input_id")))
					if err != nil || c.SHA256Hex(data) != mask.Str("sha256") {
						return c.Err(410, "input_unavailable", "Mask bytes changed or disappeared before dispatch")
					}
					uploaded, err := client.JSON("/v1/inputs", "POST", data, "image/png")
					if err != nil {
						return err
					}
					up := uploaded.(*jsonv.Object)
					if up.Str("sha256") != mask.Str("sha256") {
						return c.Err(503, "protocol_mismatch", "Worker mask digest mismatch")
					}
					op.Set("worker_mask_id", up.Get("input_id"))
					if err := r.save(job, nil); err != nil {
						return err
					}
				}
				inputs.Set("mask", op.Get("worker_mask_id"))
			}

			if overlay := op.Obj("overlay"); overlay != nil {
				if op.Str("worker_overlay_id") == "" {
					data, err := os.ReadFile(r.InputPath(overlay.Str("input_id")))
					if err != nil || c.SHA256Hex(data) != overlay.Str("sha256") {
						return c.Err(410, "input_unavailable", "Overlay bytes changed or disappeared before dispatch")
					}
					uploaded, err := client.JSON("/v1/inputs", "POST", data, "image/png")
					if err != nil {
						return err
					}
					up := uploaded.(*jsonv.Object)
					if up.Str("sha256") != overlay.Str("sha256") {
						return c.Err(503, "protocol_mismatch", "Worker overlay digest mismatch")
					}
					op.Set("worker_overlay_id", up.Get("input_id"))
					if err := r.save(job, nil); err != nil {
						return err
					}
				}
				inputs.Set("overlay", op.Get("worker_overlay_id"))
			}
			if err := r.uploadControlImage(client, job, op, inputs); err != nil {
				return err
			}
			if err := r.uploadReference(client, job, op, inputs); err != nil {
				return err
			}
			snapshot, err = client.Obj("/v1/executions", "POST", jsonv.O(
				"protocol", protocol,
				"attempt_id", op.Get("attempt_id"),
				"bundle_digest", op.Obj("model").Obj("bundle").Get("bundle_digest"),
				"task", op.Get("task"),
				"inputs", inputs,
				"parameters", op.Get("parameters"),
			))
			if err != nil {
				return err
			}
		}
		if !jsonEqual(snapshot.Get("session_id"), op.Get("session_id")) || !jsonEqual(snapshot.Get("attempt_id"), op.Get("attempt_id")) {
			return c.Err(503, "protocol_mismatch", "Worker execution identity mismatch")
		}
		op.Set("execution_id", jsonv.Maybe(snapshot.Get("execution_id")))
		if err := r.save(job, nil); err != nil {
			return err
		}
	} else if snapshot, err = client.Obj("/v1/executions/"+op.Str("execution_id"), "GET", nil); err != nil {
		return err
	}
	r.refresh(job)
	if truthy(job.Get("cancel_requested")) && !c.Terminal(snapshot.Str("state")) {
		if snapshot, err = client.Obj("/v1/executions/"+op.Str("execution_id")+"/cancel", "POST", jsonv.NewObject()); err != nil {
			return err
		}
	}
	err = r.save(job, func(tx *store.Store) error {
		for _, raw := range snapshot.Arr("events") {
			event, _ := raw.(*jsonv.Object)
			sequence, _ := event.Num("sequence")
			current, _ := op.Num("worker_sequence")
			if sequence <= current {
				continue
			}
			if sequence != current+1 {
				return c.Err(503, "event_gap", "Worker event history has a gap")
			}
			data := annotate(job, op, index, event.Get("data"))
			switch event.Str("type") {
			case "preview":
				if preview := rememberPreview(job, op, m.ID, event.Obj("data")); preview != nil {
					tx.Event(job, "preview", annotate(job, op, index, preview))
				}
			case "stage":
				job.Set("stage", jsonv.Maybe(event.Obj("data").Get("stage")))
				job.Delete("progress")
				tx.Event(job, "stage", data)
			case "progress":
				job.Set("progress", jsonv.Maybe(data))
				tx.Event(job, "progress", data)
			}
			op.Set("worker_sequence", sequence)
			mirror(job)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if state := snapshot.Str("state"); overdue && (state == "succeeded" || state == "failed") {
		// The remote worker finished after the cancellation deadline instead of confirming the
		// cancellation: the job is interrupted, and nothing it produced is committed.
		r.finish(job, "failed", jsonv.O("code", "execution_interrupted",
			"message", "Remote worker reported "+state+" after the cancellation deadline; nothing was committed and the worker was not terminated",
			"forced_termination", false))
		return nil
	}
	switch snapshot.Str("state") {
	case "succeeded":
		return r.commitOutputs(client, job, snapshot)
	case "failed":
		code := snapshot.Obj("error").Str("code")
		if code == "" {
			code = "execution_failed"
		}
		r.finish(job, "failed", jsonv.O("code", code, "message", "Worker execution failed", "restart_required", truthy(snapshot.Obj("error").Get("restart_required"))))
	case "cancelled":
		r.finish(job, "cancelled", nil)
	}
	return nil
}

// dispatch persists an operation's dispatch intent: a unique attempt bound to the worker session.
func dispatch(op, caps *jsonv.Object, session any) {
	op.Set("state", "running")
	op.Obj("backend").Set("worker_version", jsonv.Maybe(caps.Get("worker_version")))
	op.Set("attempt_id", "attempt_"+c.UUID())
	op.Set("session_id", jsonv.Maybe(session))
	op.Set("dispatch_phase", "loading")
}

// operationInput is the verified image an operation hands the worker: the submitted input, or
// an earlier operation's committed output, read back from durable storage.
func (r *Runtime) operationInput(job, op *jsonv.Object) ([]byte, string, error) {
	var path, digest string
	if from := op.Obj("input_from"); from != nil {
		output := operationOutput(job, from.Str("op_id"), from.Str("output_id"))
		if output == nil {
			return nil, "", c.Err(410, "input_unavailable", "The bound output of operation "+from.Str("op_id")+" is not committed")
		}
		path, digest = r.OutputFile(job.Str("job_id"), output), output.Str("sha256")
	} else {
		record := op.Obj("input")
		path, digest = r.InputPath(record.Str("input_id")), record.Str("sha256")
	}
	bytes, err := os.ReadFile(path)
	if err != nil || c.SHA256Hex(bytes) != digest {
		return nil, "", c.Err(410, "input_unavailable", "Input bytes changed or disappeared before dispatch")
	}
	return bytes, digest, nil
}

// operationOutput is the committed output of an operation by the worker's output ID.
func operationOutput(job *jsonv.Object, opID, outputID string) *jsonv.Object {
	for _, op := range Operations(job) {
		if op.Str("op_id") != opID {
			continue
		}
		for _, raw := range op.Arr("outputs") {
			if o, ok := raw.(*jsonv.Object); ok && nullish(o.Get("operation_output_id"), o.Get("output_id")) == outputID {
				return o
			}
		}
	}
	return nil
}

// atomicFile writes a temporary file, fsyncs it and renames it into place.
