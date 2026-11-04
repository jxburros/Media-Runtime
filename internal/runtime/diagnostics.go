package runtime

import (
	"errors"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
	"math"
	"path/filepath"
	"strings"
	"syscall"
)

func workerReport(view pool.View) *jsonv.Object {
	m := view.Member
	var w *jsonv.Object
	if view.Ready() {
		p := view.Probe
		info, caps := p.Info, p.Caps
		w = jsonv.O(
			"worker_id", m.ID,
			"protocol", p.Protocol,
			"supported_protocols", nullish(info.Get("supported_protocols"), []any{info.Get("protocol")}),
			"worker_version", jsonv.Maybe(info.Get("worker_version")),
			"backend_id", caps.Get("backend_id"),
			"ownership", nullish(caps.Get("ownership"), "reference"),
			"owned_process", len(m.Config.Command) > 0,
			"session_id", jsonv.Maybe(info.Get("session_id")),
			"state", jsonv.Maybe(info.Get("state")),
			"service_ready", jsonv.Maybe(info.Get("service_ready")),
			"device", jsonv.Maybe(caps.Get("device")),
			"device_ready", jsonv.Maybe(caps.Get("ready")),
			"missing_dependencies", nullish(caps.Get("missing_dependencies"), []any{}),
			"dependencies", nullish(caps.Get("dependencies"), jsonv.NewObject()),
			"tasks", nullish(caps.Get("tasks"), []any{"text-to-image"}),
			"adapters", nullish(caps.Get("adapters")),
			"precisions", jsonv.Maybe(caps.Get("precisions")),
			"load_defaults", jsonv.Maybe(caps.Get("load_defaults")),
			"memory_budget_mb", nullish(caps.Get("memory_budget_mb")),
			"memory_estimate", nullish(caps.Get("memory_estimate")),
			"loaded_bundle_digest", nullish(info.Obj("loaded_bundle").Get("bundle_digest")),
		)
		if c.Minor(p.Protocol) >= 2 {
			w.Set("device_identity", nullish(info.Get("device_identity")))
			w.Set("loaders", nullish(caps.Get("loaders"), []any{}))
			w.Set("auto_options", nullish(caps.Get("auto_options"), []any{}))
			w.Set("fallbacks", nullish(caps.Get("fallbacks"), []any{}))
			w.Set("applied_fallbacks", nullish(info.Obj("loaded_bundle").Get("applied_fallbacks"), []any{}))
		}
		w.Set("ready", truthy(info.Get("service_ready")) && truthy(caps.Get("ready")))
	} else {
		code, message := "backend_unavailable", "Worker is unavailable"
		var api *c.ApiError
		if errors.As(view.Err, &api) {
			code, message = api.Code, api.Message
		}
		w = jsonv.O("worker_id", m.ID, "error", code, "message", message, "owned_process", len(m.Config.Command) > 0, "ready", false)
		if view.Conflict != "" {
			w.Set("duplicate_of", view.Conflict)
		}
	}
	// How the worker is reached: loopback HTTP, or mutual TLS to a remote worker with the
	// fingerprint it must present (pinned or recorded on first contact) and the one it presented.
	t := m.Client.Transport()
	w.Set("transport", map[bool]string{true: "mtls", false: "http"}[t.Remote])
	w.Set("remote", t.Remote)
	var expected, presented, latency any
	if t.Expected != "" {
		expected = t.Expected
	}
	if t.Presented != "" {
		presented = t.Presented
	}
	if t.Latency > 0 {
		latency = math.Round(float64(t.Latency.Microseconds())/10) / 100
	}
	if t.Remote {
		source := "first_contact"
		if t.Pinned {
			source = "pinned"
		}
		w.Set("certificate_sha256", expected)
		w.Set("certificate_source", source)
		w.Set("presented_sha256", presented)
		if view.Ready() {
			w.Set("ownership_available", nullish(view.Probe.Info.Get("ownership_available")))
		}
	}
	w.Set("latency_ms", latency)
	// The operator's statement of intent, beside what the worker reports.
	w.Set("configured_device", nullish(m.Config.Device))
	var budget any
	if m.Config.MemoryBudgetMB > 0 {
		budget = float64(m.Config.MemoryBudgetMB)
	}
	w.Set("configured_memory_budget_mb", budget)
	return w
}

// Doctor is the diagnostic report behind `doctor` and GET /v1/runtime: `worker` is the first
// configured worker (the only one of a legacy configuration) and `workers` lists every one.
func (r *Runtime) Doctor() (*jsonv.Object, error) {
	views := r.Pool.ProbeAll()
	reports := []any{}
	loadedOn := map[string][]any{}
	for _, view := range views {
		report := workerReport(view)
		reports = append(reports, report)
		if digest, ok := report.Get("loaded_bundle_digest").(string); ok {
			loadedOn[digest] = append(loadedOn[digest], view.Member.ID)
		}
	}
	w := reports[0].(*jsonv.Object)
	available, err := statfs(r.Config.Directory)
	if err != nil {
		return nil, err
	}
	writable := syscall.Access(r.Config.Directory, 2) == nil
	models := []any{}
	for _, model := range r.Store.RegistryAll("models") {
		root := r.Config.Catalog(model.Str("artifact_handle"))
		missing := []string{}
		seen := map[string]bool{}
		bundle := model.Obj("bundle")
		for _, relative := range bundle.Obj("configuration").Obj("files").Keys() {
			if root == "" || !exists(filepath.Join(root, relative)) {
				role := strings.Split(relative, "/")[0]
				if !seen[role] {
					seen[role] = true
					missing = append(missing, role)
				}
			}
		}
		roles := make([]any, len(missing))
		for i, m := range missing {
			roles[i] = m
		}
		models = append(models, jsonv.O(
			"model_id", model.Get("model_id"),
			"artifact_handle", model.Get("artifact_handle"),
			"family", nullish(bundle.Get("family"), "sdxl-base"),
			"scale", jsonv.Maybe(model.Get("scale")),
			"video_sizes", jsonv.Maybe(model.Get("video_sizes")),
			"default_frames", jsonv.Maybe(model.Get("default_frames")),
			"sample_rate", jsonv.Maybe(model.Get("sample_rate")),
			"channels", jsonv.Maybe(model.Get("channels")),
			"duration_range", jsonv.Maybe(model.Get("duration_range")),
			"conversion", jsonv.Maybe(bundle.Obj("source").Get("conversion")),
			"bundle_digest", bundle.Get("bundle_digest"),
			"available", len(missing) == 0,
			"missing_roles", roles,
			"loaded", len(loadedOn[bundle.Str("bundle_digest")]) > 0,
			"loaded_on", append([]any{}, loadedOn[bundle.Str("bundle_digest")]...),
		))
	}
	adapters := []any{}
	for _, adapter := range r.Store.RegistryAll("adapters") {
		root := r.Config.Catalog(adapter.Str("artifact_handle"))
		ok := true
		for _, relative := range adapter.Obj("files").Keys() {
			if root == "" || !exists(filepath.Join(root, relative)) {
				ok = false
			}
		}
		adapters = append(adapters, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "available", ok))
	}
	recipes := []any{}
	for _, recipe := range r.Store.RegistryAll("recipes") {
		entry := jsonv.O("recipe_id", recipe.Get("recipe_id"), "pipeline", recipe.Get("pipeline"))
		if c.IsLinear(recipe) {
			// A linear recipe's pipeline is its operations' tasks in order.
			tasks := []string{}
			for _, raw := range recipe.Arr("operations") {
				tasks = append(tasks, raw.(*jsonv.Object).Str("task"))
			}
			entry.Set("pipeline", strings.Join(tasks, " > ")).Set("operations", float64(len(tasks)))
		}
		recipes = append(recipes, entry)
	}
	queued, running := 0, 0
	for _, j := range r.Store.All("") {
		switch j.Str("state") {
		case "queued":
			queued++
		case "running":
			running++
		}
	}
	var retention any
	if r.Config.OutputRetentionDays != nil {
		retention = *r.Config.OutputRetentionDays
	}
	protocols := make([]any, len(c.PROTOCOLS))
	for i, p := range c.PROTOCOLS {
		protocols[i] = p
	}
	return jsonv.O(
		"runtime_version", c.VERSION,
		"protocols", protocols,
		"worker", w,
		"workers", reports,
		"models", models,
		"adapters", adapters,
		"recipes", recipes,
		"storage", jsonv.O("writable", writable, "available_bytes", float64(available), "output_retention_days", retention),
		"queue", jsonv.O("capacity", float64(r.Config.QueueCapacity), "queued", float64(queued), "running", float64(running)),
		"queue_capacity", float64(r.Config.QueueCapacity),
	), nil
}

// Workers is GET /v1/workers: every configured worker with its device identity, readiness and
// loaded identity, whether it is running a job, and that job's ID only when the job belongs to
// the caller's namespace.
func (r *Runtime) Workers(namespace string) []any {
	views := r.Pool.ProbeAll()
	running := map[string]*jsonv.Object{}
	for _, job := range r.Store.All("running") {
		running[job.Str("worker_id")] = job
	}
	models := r.Store.RegistryAll("models")
	list := []any{}
	for _, view := range views {
		m := view.Member
		report := workerReport(view)
		entry := jsonv.O(
			"worker_id", m.ID,
			"ready", report.Get("ready"),
			"state", nullish(report.Get("state")),
			"protocol", nullish(report.Get("protocol")),
			"backend_id", nullish(report.Get("backend_id")),
			"device", nullish(report.Get("device")),
			"device_identity", nullish(report.Get("device_identity")),
			"configured_device", report.Get("configured_device"),
			"configured_memory_budget_mb", report.Get("configured_memory_budget_mb"),
			"owned_process", report.Get("owned_process"),
			"transport", report.Get("transport"),
			"remote", report.Get("remote"),
			"certificate_sha256", nullish(report.Get("certificate_sha256")),
			"latency_ms", report.Get("latency_ms"),
		)
		var loaded any
		if view.Ready() {
			if bundle := view.Probe.Info.Obj("loaded_bundle"); bundle != nil {
				ids := []any{}
				for _, model := range models {
					if jsonEqual(model.Obj("bundle").Get("bundle_digest"), bundle.Get("bundle_digest")) {
						ids = append(ids, model.Get("model_id"))
					}
				}
				adapters := []any{}
				for _, raw := range asArray(bundle.Get("adapters")) {
					if a, ok := raw.(*jsonv.Object); ok {
						adapters = append(adapters, jsonv.O("adapter_id", nullish(a.Get("adapter_id")), "digest", nullish(a.Get("digest")), "scale", nullish(a.Get("scale"))))
					}
				}
				loaded = jsonv.O("bundle_digest", nullish(bundle.Get("bundle_digest")), "model_ids", ids, "options", nullish(bundle.Get("options")), "adapters", adapters)
			}
		}
		entry.Set("loaded", loaded)
		job := running[m.ID]
		entry.Set("busy", job != nil)
		var jobID any
		if job != nil && job.Str("namespace") == namespace {
			jobID = job.Get("job_id")
		}
		entry.Set("running_job_id", jobID)
		if report.Has("error") {
			entry.Set("error", report.Get("error")).Set("message", report.Get("message"))
		}
		list = append(list, entry)
	}
	return list
}

// Cleanup removes expired outputs, including a linear job's intermediates and the committed
// operation outputs of a job that failed later, and expired inputs no active job references.
