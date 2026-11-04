package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/security"
)

func (q *request) requirePermission(permission string) error {
	if q.key != nil && q.key.Allows(permission) {
		return nil
	}
	return q.requireOperator()
}
func permissionFor(path, method string) string {
	switch {
	case strings.HasPrefix(path, "/v1/keys"), strings.HasPrefix(path, "/v1/pairing"), path == "/v1/diagnostics", path == "/metrics", path == "/v1/openapi.json":
		return "admin"
	case path == "/v1/models" && method == "POST":
		return "models.register"
	case strings.HasPrefix(path, "/v1/models") && method == "GET":
		return "models.read"
	case strings.HasPrefix(path, "/v1/jobs") || strings.HasPrefix(path, "/v1/inputs"):
		if method == "GET" {
			return "jobs.read"
		}
		return "jobs.submit"
	case (path == "/v1/runtime" || path == "/v1/workers" || strings.HasPrefix(path, "/v1/recipes") || path == "/v1/adapters") && method == "GET":
		return "jobs.read"
	default:
		return "admin"
	}
}
func jsonObject(value any) any {
	data, _ := json.Marshal(value)
	out, _ := jsonv.Parse(data)
	return out
}

func (s *Server) descriptor() *jsonv.Object {
	return jsonv.O("schemaVersion", 1.0, "name", "Media Runtime", "version", c.VERSION,
		"endpoint", jsonv.O("scheme", s.Scheme(), "host", s.host, "port", float64(s.Port())),
		"transport", s.Scheme(), "fingerprint", jsonv.Maybe(s.Fingerprint()), "authRequired", true,
		"authMode", "keys", "authRequiredForLocal", true, "pairingSupported", true, "api", "/v1",
		"surfaces", []any{"media.jobs", "media.models", "media.recipes", "media.inputs", "media.workers", "management.keys", "management.pairing", "management.metrics", "management.diagnostics"})
}
func (s *Server) securityRoutes(q *request, path, method string) (bool, error) {
	matched := path == "/v1/openapi.json" || path == "/v1/diagnostics" || path == "/metrics" || path == "/v1/keys" || path == "/v1/pairing" || strings.HasPrefix(path, "/v1/keys/")
	if !matched {
		return false, nil
	}
	if err := q.requireOperator(); err != nil {
		return true, err
	}
	manager := s.Runtime.Security
	if path == "/v1/openapi.json" && method == "GET" {
		return true, q.send(200, OpenAPI())
	}
	if path == "/v1/diagnostics" && method == "GET" {
		return true, q.send(200, s.diagnostics())
	}
	if path == "/metrics" && method == "GET" {
		return true, s.metrics(q)
	}
	if path == "/v1/keys" && method == "GET" {
		keys, err := manager.List()
		if err != nil {
			return true, err
		}
		return true, q.send(200, jsonObject(keys))
	}
	if (path == "/v1/keys" || path == "/v1/pairing") && method == "POST" {
		if err := q.requireOperator(); err != nil {
			return true, err
		}
		body, err := q.raw(64 * 1024)
		if err != nil {
			return true, err
		}
		p, err := security.DecodePolicy(body)
		if err != nil {
			return true, c.Err(400, "invalid_request", err.Error())
		}
		if path == "/v1/pairing" {
			code, err := manager.Pair(p)
			if err != nil {
				return true, err
			}
			return true, q.send(201, jsonv.O("code", code, "expires_in", 300.0, "fingerprint", jsonv.Maybe(s.Fingerprint())))
		}
		k, token, err := manager.Create(p)
		if err != nil {
			return true, err
		}
		return true, q.send(201, jsonv.O("key", jsonObject(k), "token", token))
	}
	if strings.HasPrefix(path, "/v1/keys/") && method == "DELETE" {
		if err := q.requireOperator(); err != nil {
			return true, err
		}
		id := strings.TrimPrefix(path, "/v1/keys/")
		if !regexp.MustCompile(`^key_[a-f0-9]{32}$`).MatchString(id) {
			return true, c.Err(404, "not_found", "Key not found")
		}
		if err := manager.Revoke(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return true, c.Err(404, "not_found", "Key not found")
			}
			return true, err
		}
		return true, q.send(200, jsonv.O("revoked", true))
	}
	return false, nil
}
func (s *Server) redeem(q *request) error {
	peer, _, _ := net.SplitHostPort(q.r.RemoteAddr)
	// Redeeming is intentionally possible without an existing key. A separate low
	// rate budget limits code guessing; the code itself has 128 random bits.
	if ok, retry := s.Runtime.Security.Keys.Allow("pair:"+peer, 10); !ok {
		q.w.Header().Set("Retry-After", fmt.Sprint(retry))
		return c.Err(429, "pairing_rate_limited", "Too many pairing attempts")
	}
	body, err := q.body()
	if err != nil {
		return err
	}
	object, ok := body.(*jsonv.Object)
	if !ok || object.Len() != 1 || len(object.Str("code")) != 32 {
		return c.Err(400, "invalid_request", "Expected a pairing code", "code")
	}
	key, token, err := s.Runtime.Security.Redeem(object.Str("code"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.Err(401, "unauthorized", "Pairing code is expired or unavailable")
		}
		return err
	}
	return q.send(201, jsonv.O("key", jsonObject(key), "token", token, "fingerprint", jsonv.Maybe(s.Fingerprint())))
}

func (s *Server) repair(q *request, api *c.ApiError) *jsonv.Object {
	if api.Extra != nil && api.Extra.Obj("repair") != nil {
		return api.Extra.Obj("repair")
	}
	if api.Code == "key_queue_full" || api.Code == "queue_full" {
		return jsonv.O("label", "View queued jobs", "method", "GET", "path", "/v1/jobs")
	}
	if api.Field == "parameters.count" && api.Code == "invalid_request" {
		return jsonv.O("label", fmt.Sprintf("Use at most %d outputs", c.MaxCount), "method", "POST", "path", "/v1/jobs/validate", "patch", jsonv.O("parameters", jsonv.O("count", float64(c.MaxCount))))
	}
	if api.Code == "resource_exhausted" && q.fallbacks {
		return jsonv.O("label", "Validate with an authorized CPU fallback", "method", "POST", "path", "/v1/jobs/validate", "patch", jsonv.O("authorized_fallbacks", []any{"cpu"}))
	}
	return nil
}

// Diagnostics is built from an allow-list; it never serializes configuration,
// worker errors, filesystem paths, job requests, inputs, outputs or manifests.
func (s *Server) diagnostics() *jsonv.Object {
	workers := []any{}
	for _, v := range s.Runtime.Pool.Views() {
		w := jsonv.O("worker_id", v.Member.ID, "ready", v.Ready())
		if v.Probe != nil {
			w.Set("protocol", v.Probe.Protocol).Set("backend_id", v.Probe.Caps.Get("backend_id")).Set("worker_version", v.Probe.Info.Get("worker_version"))
		}
		workers = append(workers, w)
	}
	counts := jsonv.NewObject()
	for _, state := range []string{"queued", "running", "succeeded", "failed", "cancelled"} {
		counts.Set(state, float64(len(s.Runtime.Store.All(state))))
	}
	return jsonv.O("schema_version", 1.0, "runtime_version", c.VERSION, "generated_at", c.Now(), "jobs", counts, "workers", workers)
}

var metricLine = regexp.MustCompile(`^(media_engine_[a-zA-Z0-9_]+)(\{[^\n]*\})? ([0-9.eE+\-]+)$`)

func (s *Server) metrics(q *request) error {
	var out strings.Builder
	counts := map[string]int{}
	outputs := map[string]float64{}
	phases := map[string]float64{}
	for _, j := range s.Runtime.Store.All("") {
		recipe := j.Obj("recipe").Str("recipe_id")
		label := fmt.Sprintf("recipe=%q,state=%q", recipe, j.Str("state"))
		counts[label]++
		for _, raw := range j.Arr("outputs") {
			o, _ := raw.(*jsonv.Object)
			size, _ := o.Num("size")
			outputs[recipe] += size
		}
		stage := ""
		var start time.Time
		for _, event := range s.Runtime.Store.Events(j.Str("job_id"), 0) {
			at, err := time.Parse(time.RFC3339Nano, event.Str("timestamp"))
			if err != nil {
				continue
			}
			next := event.Obj("data").Str("stage")
			terminal := event.Str("type") == "job_state" && c.Terminal(event.Obj("data").Str("state"))
			if (event.Str("type") == "stage" && next != stage) || terminal {
				if stage != "" && !start.IsZero() {
					phases[stage] += at.Sub(start).Seconds()
				}
				stage = next
				start = at
			}
		}
	}
	out.WriteString("# TYPE media_runtime_jobs gauge\n")
	keys := []string{}
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&out, "media_runtime_jobs{%s} %d\n", k, counts[k])
	}
	fmt.Fprintf(&out, "# TYPE media_runtime_queue_depth gauge\nmedia_runtime_queue_depth %d\n", len(s.Runtime.Store.All("queued")))
	for recipe, n := range outputs {
		fmt.Fprintf(&out, "media_runtime_output_bytes{recipe=%q} %g\n", recipe, n)
	}
	for phase, n := range phases {
		fmt.Fprintf(&out, "media_runtime_phase_seconds{phase=%q} %g\n", phase, n)
	}
	for _, view := range s.Runtime.Pool.Views() {
		id := view.Member.ID
		busy := 0
		if view.Probe != nil && view.Probe.Info.Str("state") == "busy" {
			busy = 1
		}
		fmt.Fprintf(&out, "media_runtime_worker_busy{worker=%q} %d\n", id, busy)
		res, err := view.Member.Client.Response("/metrics", "GET", nil, "")
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
		res.Body.Close()
		if err != nil || len(body) > 1024*1024 {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			m := metricLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			labels := strings.TrimSuffix(strings.TrimPrefix(m[2], "{"), "}")
			if labels != "" {
				labels += ","
			}
			fmt.Fprintf(&out, "%s{%sworker=%q} %s\n", m[1], labels, id, m[3])
		}
	}
	q.w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	q.w.WriteHeader(200)
	q.sent = true
	_, err := io.WriteString(q.w, out.String())
	return err
}

// A model scope covers every operation, including unpublished intermediates.
func (q *request) canReadJob(job *jsonv.Object) bool {
	if q.key == nil {
		return true
	}
	if !q.key.AllowsModel(job.Obj("model").Str("model_id")) {
		return false
	}
	for _, raw := range job.Arr("operations") {
		op, _ := raw.(*jsonv.Object)
		if !q.key.AllowsModel(op.Obj("model").Str("model_id")) {
			return false
		}
	}
	return true
}
func (q *request) filterDiagnostics(report *jsonv.Object) *jsonv.Object {
	if q.key == nil || len(q.key.Models) == 0 {
		return report
	}
	out := report.Clone()
	models := []any{}
	for _, raw := range out.Arr("models") {
		model, _ := raw.(*jsonv.Object)
		if q.key.AllowsModel(model.Str("model_id")) {
			models = append(models, raw)
		}
	}
	out.Set("models", models)
	return out
}
func (s *Server) scopedWorkers(q *request, list []any) []any {
	if q.key == nil || len(q.key.Models) == 0 {
		return list
	}
	for _, raw := range list {
		w, _ := raw.(*jsonv.Object)
		loaded := w.Obj("loaded")
		if loaded != nil {
			ids := []any{}
			for _, id := range loaded.Arr("model_ids") {
				name, _ := id.(string)
				if q.key.AllowsModel(name) {
					ids = append(ids, name)
				}
			}
			if len(ids) == 0 {
				w.Set("loaded", nil)
			} else {
				loaded.Set("model_ids", ids)
			}
		}
		if id := w.Str("running_job_id"); id != "" {
			job := s.Runtime.Store.Get(id, q.namespace)
			if job == nil || !q.canReadJob(job) {
				w.Set("running_job_id", nil)
			}
		}
	}
	return list
}
