package main

import (
	"fmt"
	"strings"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// text formats a member the way a JavaScript template literal interpolates it.
func text(o *jsonv.Object, key string) string {
	if !o.Has(key) {
		return "undefined"
	}
	return jsText(o.Get(key))
}

func jsText(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case float64:
		return jsonv.Number(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e != nil {
				parts[i] = jsText(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// or is `o.key ?? fallback` interpolated.
func or(o *jsonv.Object, key, fallback string) string {
	if o.Get(key) == nil {
		return fallback
	}
	return jsText(o.Get(key))
}

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

func joinList(v any, sep string) string {
	list, _ := v.([]any)
	parts := make([]string, len(list))
	for i, e := range list {
		if e != nil {
			parts[i] = jsText(e)
		}
	}
	return strings.Join(parts, sep)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// short abbreviates a digest for display.
func short(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func objOrEmpty(o *jsonv.Object) *jsonv.Object {
	if o == nil {
		return jsonv.NewObject()
	}
	return o
}

// ExplainDoctor turns the diagnostic report into operator guidance and an overall verdict.
func ExplainDoctor(report *jsonv.Object) ([]string, bool) {
	lines := []string{}
	healthy := true
	lines = append(lines, fmt.Sprintf("Media Runtime %s; protocols %s", text(report, "runtime_version"), joinList(report.Get("protocols"), ", ")))
	w := objOrEmpty(report.Obj("worker"))
	// A pool of one keeps the single worker section; a pool prints one section per worker.
	workers := report.Arr("workers")
	pool := len(workers) > 1
	if pool {
		for _, raw := range workers {
			entry := objOrEmpty(raw.(*jsonv.Object))
			if !explainWorker(&lines, entry, "Worker "+text(entry, "worker_id"), "workers[].command") {
				healthy = false
			}
		}
	} else if !explainWorker(&lines, w, "Worker", "worker.command") {
		healthy = false
	}
	models := report.Arr("models")
	if len(models) == 0 {
		lines = append(lines, `Models: none registered — run "models inspect" then "models register".`)
		healthy = false
	}
	for _, raw := range models {
		m := objOrEmpty(raw.(*jsonv.Object))
		if !truthy(m.Get("available")) {
			healthy = false
		}
		state := "MISSING " + joinList(m.Get("missing_roles"), ", ")
		if truthy(m.Get("available")) {
			state = "all registered files present"
		}
		loaded := ""
		if truthy(m.Get("loaded")) {
			loaded = "; loaded in the worker"
			if pool {
				loaded = "; loaded in worker " + joinList(m.Get("loaded_on"), ", ")
			}
		}
		kind := ""
		if f := m.Str("family"); f != "" && f != "sdxl-base" {
			kind = ", " + f
			if m.Get("scale") != nil {
				kind += " ×" + text(m, "scale")
			}
		}
		lines = append(lines, fmt.Sprintf("Model %s (%s%s): %s%s", text(m, "model_id"), text(m, "artifact_handle"), kind, state, loaded))
		if conversion := m.Obj("conversion"); conversion != nil {
			lines = append(lines, fmt.Sprintf("  converted from source sha256 %s (reference %s)", short(conversion.Str("source_sha256")), short(conversion.Str("reference_bundle_digest"))))
		}
	}
	for _, raw := range report.Arr("adapters") {
		a := objOrEmpty(raw.(*jsonv.Object))
		if !truthy(a.Get("available")) {
			healthy = false
		}
		state := "MISSING files"
		if truthy(a.Get("available")) {
			state = "files present"
		}
		lines = append(lines, fmt.Sprintf("Adapter %s: %s", text(a, "adapter_id"), state))
	}
	if w.Has("adapters") && w.Get("adapters") == nil {
		lines = append(lines, "  LoRA adapters: not advertised by this worker")
	}
	recipes := []string{}
	for _, raw := range report.Arr("recipes") {
		r := objOrEmpty(raw.(*jsonv.Object))
		recipes = append(recipes, fmt.Sprintf("%s (%s)", text(r, "recipe_id"), text(r, "pipeline")))
	}
	lines = append(lines, "Recipes: "+strings.Join(recipes, ", "))
	s := objOrEmpty(report.Obj("storage"))
	if !truthy(s.Get("writable")) {
		healthy = false
	}
	writable := "NOT WRITABLE"
	if truthy(s.Get("writable")) {
		writable = "writable"
	}
	free, _ := s.Num("available_bytes")
	days := ""
	if truthy(s.Get("output_retention_days")) {
		days = " days"
	}
	lines = append(lines, fmt.Sprintf("Storage: %s, %.1f GiB free; output retention %s%s", writable, free/(1<<30), or(s, "output_retention_days", "unlimited"), days))
	q := objOrEmpty(report.Obj("queue"))
	capacity := or(q, "capacity", "")
	if q.Get("capacity") == nil {
		capacity = text(report, "queue_capacity")
	}
	lines = append(lines, fmt.Sprintf("Queue: %s running, %s queued, capacity %s", or(q, "running", "0"), or(q, "queued", "0"), capacity))
	if healthy {
		lines = append(lines, "Result: ready to generate.")
	} else {
		lines = append(lines, "Result: attention needed (see lines marked NOT/MISSING/UNAVAILABLE).")
	}
	return lines, healthy
}

// explainWorker appends one worker's section under its label and reports whether it is healthy.
func explainWorker(out *[]string, w *jsonv.Object, label, command string) bool {
	lines := *out
	healthy := true
	if truthy(w.Get("error")) {
		healthy = false
		lines = append(lines, fmt.Sprintf("%s: UNAVAILABLE (%s) — %s", label, text(w, "error"), or(w, "message", "")))
		lines = append(lines, "  Start Media Engine (or set "+command+") and check its token and port.")
	} else {
		process := "externally managed"
		if truthy(w.Get("owned_process")) {
			process = "owned child process"
		}
		lines = append(lines, fmt.Sprintf("%s: %s %s (%s stage), negotiated %s, %s", label, text(w, "backend_id"), text(w, "worker_version"), text(w, "ownership"), text(w, "protocol"), process))
		ready := "NOT READY"
		if truthy(w.Get("device_ready")) {
			ready = "ready"
		}
		lines = append(lines, fmt.Sprintf("  Device %s: %s; service state %s; tasks %s", text(w, "device"), ready, text(w, "state"), joinList(w.Get("tasks"), ", ")))
		if !truthy(w.Get("device_ready")) {
			healthy = false
		}
		if id := w.Obj("device_identity"); id != nil {
			memory := "memory unknown"
			if id.Get("total_memory_mb") != nil {
				memory = text(id, "total_memory_mb") + " MiB"
			}
			host := id.Str("host_id")
			if len(host) > 12 {
				host = host[:12]
			}
			lines = append(lines, fmt.Sprintf("  Device identity: %s (%s), %s, host %s, pid %s", or(id, "name", "unnamed"), or(id, "device", "?"), memory, host, text(id, "pid")))
		}
		if loaders := w.Arr("loaders"); len(loaders) > 0 {
			parts := []string{}
			for _, raw := range loaders {
				l := objOrEmpty(raw.(*jsonv.Object))
				parts = append(parts, fmt.Sprintf("%s (%s)", joinList(l.Get("families"), "/"), text(l, "format")))
			}
			lines = append(lines, "  Loaders: "+strings.Join(parts, ", "))
		}
		if w.Has("auto_options") || w.Has("fallbacks") {
			lines = append(lines, fmt.Sprintf("  Automatic options: %s; authorizable fallbacks: %s", orNone(joinList(w.Get("auto_options"), ", ")), orNone(joinList(w.Get("fallbacks"), ", "))))
		}
		if missing := w.Arr("missing_dependencies"); len(missing) > 0 {
			healthy = false
			lines = append(lines, fmt.Sprintf("  Missing worker dependencies: %s — install the inference extra in Engine's environment.", joinList(missing, ", ")))
		}
		deps := []string{}
		dependencies := w.Obj("dependencies")
		for _, k := range dependencies.Keys() {
			deps = append(deps, k+"="+or(dependencies, k, "missing"))
		}
		if len(deps) > 0 {
			lines = append(lines, "  Dependencies: "+strings.Join(deps, " "))
		}
		defaults := "undefined"
		if w.Has("load_defaults") {
			defaults = string(jsonv.MustMarshal(w.Get("load_defaults")))
		}
		lines = append(lines, fmt.Sprintf("  Load defaults: %s; precisions %s; memory budget %s MiB", defaults, joinList(w.Get("precisions"), "/"), or(w, "memory_budget_mb", "not set")))
		estimate := w.Obj("memory_estimate")
		if estimate.Get("value_mb") != nil {
			lines = append(lines, fmt.Sprintf("  Memory estimate: %s MiB parameters at %s (source: %s; %s)", text(estimate, "value_mb"), text(estimate, "precision"), text(estimate, "source"), text(estimate, "uncertainty")))
		} else {
			lines = append(lines, fmt.Sprintf(`  Memory estimate: unknown (%s); run "models show MODEL --verify" for a per-model estimate.`, or(estimate, "uncertainty", "no bundle validated yet")))
		}
	}
	*out = lines
	return healthy
}

// ExplainCatalog describes catalog inspection entries for the installation workflow.
func ExplainCatalog(entries []any) []string {
	lines := []string{}
	for _, raw := range entries {
		e := objOrEmpty(raw.(*jsonv.Object))
		state := "NOT READY"
		if truthy(e.Get("complete")) {
			state = "complete SDXL base bundle"
			if e.Str("family") == "esrgan" {
				state = "complete esrgan upscaler ×" + text(e, "scale")
			}
		}
		size := ""
		if truthy(e.Get("present")) {
			total, _ := e.Num("total_bytes")
			size = fmt.Sprintf(" (%s files, %.2f GiB)", text(e, "file_count"), total/(1<<30))
		}
		lines = append(lines, fmt.Sprintf("%s: %s%s", text(e, "artifact_handle"), state, size))
		for _, problem := range e.Arr("problems") {
			lines = append(lines, "  - "+jsText(problem))
		}
		if conversion := e.Obj("conversion"); conversion != nil {
			converter := []string{}
			tools := conversion.Obj("converter")
			for _, k := range tools.Keys() {
				converter = append(converter, k+"="+jsText(tools.Get(k)))
			}
			lines = append(lines, fmt.Sprintf("  converted from source sha256 %s, reference bundle %s, by %s", short(conversion.Str("source_sha256")), short(conversion.Str("reference_bundle_digest")), strings.Join(converter, " ")))
		}
		if truthy(e.Get("complete")) {
			lines = append(lines, fmt.Sprintf("  register with: media-runtime models register <file containing %s>", jsonv.MustMarshal(e.Get("registration"))))
		}
	}
	if len(entries) == 0 {
		lines = append(lines, "No artifact handles are configured in artifact_catalog.")
	}
	return lines
}
