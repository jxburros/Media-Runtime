package runtime

import (
	"errors"
	"os"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
)

// cacheIdentity is deliberately conservative: session affinity prevents reuse after a
// worker's software/dependencies change without a version bump. No cross-credential
// or cross-namespace source can disclose private prompts or artifact identities.
func (r *Runtime) cacheIdentity(job, op *jsonv.Object) (string, error) {
	inputs := op.Obj("inputs").Clone()
	if c.UsesImage(op.Str("task")) {
		_, digest, err := r.operationInput(job, op)
		if err != nil {
			return "", err
		}
		inputs.Set("image", digest)
	}
	if from := op.Obj("control_image_from"); from != nil {
		_, digest, err := r.operationInput(job, jsonv.O("input_from", from))
		if err != nil {
			return "", err
		}
		inputs.Set("control_image", digest)
	}
	for _, role := range []string{"mask", "overlay", "control_image", "reference_image"} {
		if record := op.Obj(role); record != nil {
			data, err := os.ReadFile(r.InputPath(record.Str("input_id")))
			if err != nil || c.SHA256Hex(data) != record.Str("sha256") {
				return "", c.Err(410, "input_unavailable", "Cached operation input changed before dispatch")
			}
			inputs.Set(role, record.Str("sha256"))
		}
	}
	return c.Hash(jsonv.O("cache_schema", 1.0, "runtime_version", c.VERSION,
		"namespace", job.Get("namespace"), "key_id", job.Get("key_id"),
		"worker_id", job.Get("worker_id"), "session_id", op.Get("session_id"),
		"task", op.Get("task"), "inputs", inputs, "parameters", op.Get("parameters"),
		"bundle", op.Obj("model").Get("bundle"), "adapters", manifestAdapters(op), "controlnets", op.Get("controlnets"), "conditioning", manifestConditioning(op),
		"backend", op.Get("backend"), "applied_fallbacks", op.Get("applied_fallbacks"))), nil
}

// tryCached only runs after the worker confirms the durable attempt was never
// accepted. Final operations always execute. Sources are successful durable jobs;
// no cache files or shared storage namespace are introduced.
func (r *Runtime) tryCached(client *worker.Client, job, op *jsonv.Object, index int) (bool, error) {
	if !job.Obj("request").Bool("reuse_intermediates") || !c.IsLinear(job.Obj("recipe")) || index == len(Operations(job))-1 {
		return false, nil
	}
	digest, err := r.cacheIdentity(job, op)
	if err != nil {
		return false, err
	}
	op.Set("cache_key", digest)
	for _, source := range r.Store.All("succeeded") {
		if source.Str("job_id") == job.Str("job_id") || source.Str("namespace") != job.Str("namespace") || source.Str("key_id") != job.Str("key_id") {
			continue
		}
		for _, original := range Operations(source) {
			if original.Str("cache_key") != digest || original.Str("state") != "succeeded" || len(original.Arr("outputs")) == 0 {
				continue
			}
			outputs := []any{}
			valid := true
			for _, raw := range original.Arr("outputs") {
				record := raw.(*jsonv.Object)
				if c.Expired(record.Get("expires_at")) || !exists(r.OutputFile(source.Str("job_id"), record)) {
					valid = false
					break
				}
				copy := record.Clone().Set("output_id", nullish(record.Get("operation_output_id"), record.Get("output_id")))
				for _, field := range []string{"op_id", "operation_output_id", "published", "expires_at"} {
					copy.Delete(field)
				}
				outputs = append(outputs, copy)
			}
			if !valid {
				continue
			}
			op.Set("cache", jsonv.O("key", digest, "source_job_id", source.Get("job_id"), "source_op_id", original.Get("op_id"), "source_execution_id", original.Get("execution_id")))
			snapshot := jsonv.O("outputs", outputs, "implementation", original.Get("worker"), "effective_settings", original.Get("effective_settings"))
			// Measurements of the original execution remain explicitly attributed to it.
			op.Obj("cache").Set("source_measurements", original.Get("measurements"))
			return true, r.commitOutputs(client, job, snapshot)
		}
	}
	return false, r.save(job, nil)
}

func (r *Runtime) receiveCached(job, op, output *jsonv.Object, destination string, limit int64, check c.Signature) error {
	cache := op.Obj("cache")
	source := r.Store.Get(cache.Str("source_job_id"), job.Str("namespace"))
	if source == nil || source.Str("state") != "succeeded" || source.Str("key_id") != job.Str("key_id") {
		return errors.New("cache source no longer available")
	}
	record := operationOutput(source, cache.Str("source_op_id"), output.Str("output_id"))
	if record == nil || c.Expired(record.Get("expires_at")) || record.Str("sha256") != output.Str("sha256") {
		return errors.New("cache source expired or changed")
	}
	file, err := os.Open(r.OutputFile(source.Str("job_id"), record))
	if err != nil {
		return err
	}
	defer file.Close()
	return receiveBody(file, destination, limit, output, check)
}
