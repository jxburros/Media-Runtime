// Package api is the authenticated HTTP API and its server-sent event streams.
package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/media-runtime/internal/catalog"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/runtime"
)

// Server is the runtime's HTTP service. Close ends open event streams before shutting down.
type Server struct {
	Runtime *runtime.Runtime
	http    *http.Server
	ln      net.Listener
	closing chan struct{}
	once    sync.Once
}

func New(rt *runtime.Runtime) *Server { return NewWithTimeouts(rt, 10*time.Second, 30*time.Second) }

// NewWithTimeouts sets the header and whole-request read limits (tests shorten them).
func NewWithTimeouts(rt *runtime.Runtime, header, request time.Duration) *Server {
	s := &Server{Runtime: rt, closing: make(chan struct{})}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: header, ReadTimeout: request}
	return s
}

// Listen binds the address (port 0 picks a free port) and serves in the background.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() { _ = s.http.Serve(ln) }()
	return nil
}

func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.Port()) }

// Close ends event streams and closes every connection.
func (s *Server) Close() error {
	s.once.Do(func() { close(s.closing) })
	return s.http.Close()
}

var (
	namespacePattern = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,200}$`)
	inputPath        = regexp.MustCompile(`^/v1/inputs/(input_[a-f0-9]{32})$`)
	modelPath        = regexp.MustCompile(`^/v1/models/([a-zA-Z0-9_-]{1,80})$`)
	jobPath          = regexp.MustCompile(`^/v1/jobs/(job_[a-f0-9-]+)(?:/(events|cancel|manifest|outputs)(?:/(image))?)?$`)
)

type request struct {
	w         http.ResponseWriter
	r         *http.Request
	sent      bool
	namespace string
	operator  bool
}

func (q *request) send(status int, body any) error {
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(status)
	q.sent = true
	_, _ = q.w.Write(jsonv.MustMarshal(body))
	return nil
}

func (q *request) raw(limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(q.r.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, c.Err(413, "invalid_body", "Request body is too large")
	}
	return data, nil
}

func (q *request) body() (any, error) {
	data, err := q.raw(4 * 1024 * 1024)
	if err != nil {
		return nil, err
	}
	v, err := jsonv.Parse(data)
	if err != nil {
		return nil, c.Err(400, "invalid_json", "Expected a JSON body")
	}
	return v, nil
}

func (q *request) requireOperator() error {
	if !q.operator {
		return c.Err(403, "operator_required", "Operator permission is required")
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := c.UUID()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	q := &request{w: w, r: r}
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				err = fmt.Errorf("internal: %v", p)
			}
		}()
		return s.route(q)
	}()
	if err == nil {
		return
	}
	if q.sent {
		panic(http.ErrAbortHandler)
	}
	var api *c.ApiError
	if !errors.As(err, &api) {
		api = c.Err(500, "internal_error", "The request could not be completed")
	}
	if api.Status == 429 {
		w.Header().Set("Retry-After", "1")
	}
	body := jsonv.O("code", api.Code, "message", api.Message)
	if api.Field != "" {
		body.Set("field", api.Field)
	}
	_ = q.send(api.Status, jsonv.O("error", body, "request_id", requestID))
}

func header(r *http.Request, name string) (string, bool) {
	values, ok := r.Header[http.CanonicalHeaderKey(name)]
	if !ok {
		return "", false
	}
	return strings.Join(values, ", "), true
}

func (s *Server) route(q *request) error {
	rt := s.Runtime
	cfg := rt.Config
	r := q.r
	path := r.URL.EscapedPath()
	method := r.Method
	query := r.URL.Query()
	if _, ok := header(r, "Origin"); ok {
		return c.Err(403, "browser_origin_denied", "Use an authenticated application gateway")
	}
	if path == "/health" && method == "GET" {
		return q.send(200, jsonv.O("status", "ok"))
	}
	supplied, _ := header(r, "Authorization")
	var credential *struct {
		namespace string
		operator  bool
		allowed   []string
	}
	for _, t := range cfg.Tokens {
		expected := "Bearer " + t.Token
		if len(expected) == len(supplied) && subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1 {
			credential = &struct {
				namespace string
				operator  bool
				allowed   []string
			}{t.Namespace, t.Operator, t.AllowedNamespaces}
			break
		}
	}
	if credential == nil {
		return c.Err(401, "unauthorized", "A valid bearer credential is required")
	}
	q.namespace, q.operator = credential.namespace, credential.operator
	if suppliedNamespace, ok := header(r, "X-Media-Namespace"); ok {
		if !namespacePattern.MatchString(suppliedNamespace) {
			return c.Err(400, "invalid_namespace", "Invalid caller namespace")
		}
		allowed := false
		for _, n := range credential.allowed {
			if n == suppliedNamespace || (strings.HasSuffix(n, "*") && strings.HasPrefix(suppliedNamespace, n[:len(n)-1])) {
				allowed = true
			}
		}
		if !allowed {
			return c.Err(403, "forbidden_namespace", "Credential cannot use this namespace")
		}
		q.namespace = suppliedNamespace
	}
	namespace := q.namespace

	switch {
	case path == "/v1/runtime" && method == "GET":
		diagnostics, err := rt.Doctor()
		if err != nil {
			return err
		}
		return q.send(200, diagnostics.Clone().
			Set("api_version", "1").
			Set("limits", jsonv.O(
				"queue_capacity", float64(cfg.QueueCapacity),
				"max_input_bytes", float64(cfg.MaxInputBytes),
				"max_output_bytes", float64(cfg.MaxOutputBytes),
			)).
			Set("protocols", protocols()))
	case path == "/v1/catalog" && method == "GET":
		if err := q.requireOperator(); err != nil {
			return err
		}
		return q.send(200, catalog.Inspect(cfg.ArtifactCatalog, query.Get("handle")))
	case path == "/v1/inputs" && method == "POST":
		if ct, _ := header(r, "Content-Type"); ct != "image/png" {
			return c.Err(400, "unsupported", "Upload raw PNG bytes with Content-Type image/png", "image")
		}
		data, err := q.raw(cfg.MaxInputBytes)
		if err != nil {
			return err
		}
		record, err := rt.StoreInput(data, namespace)
		if err != nil {
			return err
		}
		return q.send(201, PublicInput(record))
	case path == "/v1/inputs" && method == "GET":
		list := []any{}
		for _, record := range rt.Store.Inputs(namespace) {
			list = append(list, PublicInput(record))
		}
		return q.send(200, list)
	}
	if m := inputPath.FindStringSubmatch(path); m != nil && (method == "GET" || method == "DELETE") {
		if method == "GET" {
			record, err := rt.OwnedInput(m[1], namespace)
			if err != nil {
				return err
			}
			return q.send(200, PublicInput(record))
		}
		if err := rt.DeleteInput(m[1], namespace); err != nil {
			return err
		}
		return q.send(200, jsonv.O("deleted", true))
	}
	if path == "/v1/models" && method == "GET" {
		models := []any{}
		for _, model := range rt.Store.RegistryAll("models") {
			bundle := model.Obj("bundle")
			_, statErr := os.Stat(cfg.Catalog(model.Str("artifact_handle")))
			models = append(models, jsonv.O(
				"model_id", model.Get("model_id"),
				"family", bundle.Get("family"),
				"tasks", []any{"text-to-image", "image-to-image"},
				"bundle_digest", bundle.Get("bundle_digest"),
				"source", jsonv.Maybe(bundle.Get("source")),
				"available", statErr == nil && cfg.Catalog(model.Str("artifact_handle")) != "",
			))
		}
		return q.send(200, models)
	}
	if m := modelPath.FindStringSubmatch(path); m != nil && method == "GET" {
		return s.modelDetail(q, m[1], query.Get("verify") == "1")
	}
	switch {
	case path == "/v1/models" && method == "POST":
		if err := q.requireOperator(); err != nil {
			return err
		}
		body, err := q.body()
		if err != nil {
			return err
		}
		model, err := catalog.BuildBundle(body, cfg.ArtifactCatalog)
		if err != nil {
			return err
		}
		p, err := rt.Worker.Probe()
		if err != nil {
			return err
		}
		validation, err := rt.Worker.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "bundle", model.Get("bundle"), "options", jsonv.Maybe(p.Caps.Get("load_defaults"))))
		if err != nil {
			return err
		}
		rt.Store.Register("models", model.Str("model_id"), model)
		return q.send(201, jsonv.O(
			"model_id", model.Get("model_id"),
			"bundle_digest", model.Obj("bundle").Get("bundle_digest"),
			"memory_estimate", nullish(validation.Get("memory_estimate")),
		))
	case path == "/v1/adapters" && method == "GET":
		list := []any{}
		for _, a := range rt.Store.RegistryAll("adapters") {
			list = append(list, jsonv.O(
				"adapter_id", a.Get("adapter_id"),
				"artifact_handle", a.Get("artifact_handle"),
				"digest", a.Get("digest"),
				"source", jsonv.Maybe(a.Get("source")),
				"files", float64(a.Obj("files").Len()),
			))
		}
		return q.send(200, list)
	case path == "/v1/adapters" && method == "POST":
		if err := q.requireOperator(); err != nil {
			return err
		}
		body, err := q.body()
		if err != nil {
			return err
		}
		adapter, err := catalog.BuildAdapter(body, cfg.ArtifactCatalog)
		if err != nil {
			return err
		}
		p, err := rt.Worker.Probe()
		if err != nil {
			return err
		}
		if !truthy(p.Caps.Get("adapters")) || p.Protocol == c.PROTOCOLS[0] {
			return c.Err(503, "unsupported", "The connected worker does not advertise LoRA adapters")
		}
		validation, err := rt.Worker.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "adapters", []any{jsonv.O("adapter", adapter, "scale", 1.0)}))
		if err != nil {
			return err
		}
		rt.Store.Register("adapters", adapter.Str("adapter_id"), adapter)
		var layout any
		if list := validation.Arr("adapters"); len(list) > 0 {
			if first, ok := list[0].(*jsonv.Object); ok {
				layout = first.Get("layout")
			}
		}
		return q.send(201, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "layout", layout))
	case path == "/v1/recipes" && method == "GET":
		list := []any{}
		for _, recipe := range rt.Store.RegistryAll("recipes") {
			list = append(list, recipe)
		}
		return q.send(200, list)
	case path == "/v1/recipes" && method == "POST":
		if err := q.requireOperator(); err != nil {
			return err
		}
		body, err := q.body()
		if err != nil {
			return err
		}
		recipe, err := c.Validate(c.RecipeSchema, body)
		if err != nil {
			return err
		}
		value := recipe.Clone().Set("digest", c.Hash(recipe))
		existing := rt.Store.Registry("recipes", recipe.Str("recipe_id"))
		revision, _ := recipe.Num("revision")
		existingRevision, _ := existing.Num("revision")
		if existing != nil && existing.Str("digest") != value.Str("digest") && revision <= existingRevision {
			return c.Err(409, "recipe_revision_conflict", "Increase revision when changing a recipe")
		}
		rt.Store.Register("recipes", recipe.Str("recipe_id"), value)
		return q.send(201, value)
	case path == "/v1/jobs/validate" && method == "POST":
		body, err := q.body()
		if err != nil {
			return err
		}
		result, err := rt.Resolve(body, namespace)
		if err != nil {
			return err
		}
		return q.send(200, jsonv.O(
			"valid", true,
			"request", result.Get("request"),
			"recipe_digest", result.Obj("recipe").Get("digest"),
			"bundle_digest", result.Obj("model").Obj("bundle").Get("bundle_digest"),
		))
	case path == "/v1/jobs" && method == "POST":
		var key *string
		if k, ok := header(r, "Idempotency-Key"); ok {
			key = &k
		}
		body, err := q.body()
		if err != nil {
			return err
		}
		job, err := rt.Submit(body, namespace, key)
		if err != nil {
			return err
		}
		return q.send(202, jsonv.O("job_id", job.Get("job_id"), "state", job.Get("state")))
	case path == "/v1/jobs" && method == "GET":
		limit, lok := jsNumber(query.Get("limit"), 50)
		after, aok := jsNumber(query.Get("after"), 0)
		if !lok || !aok || limit < 1 || limit > 200 || after < 0 {
			return c.Err(400, "invalid_pagination", "Use limit 1–200 and a nonnegative cursor")
		}
		newest := query.Get("order") == "newest"
		jobs, ordinals := rt.Store.Page(namespace, newest, int(after), int(limit))
		list := []any{}
		for _, job := range jobs {
			list = append(list, PublicJob(job))
		}
		var next any
		if !newest && len(jobs) == int(limit) {
			next = float64(ordinals[len(ordinals)-1])
		}
		return q.send(200, jsonv.O("jobs", list, "next_cursor", next))
	}
	m := jobPath.FindStringSubmatch(path)
	if m == nil {
		return c.Err(404, "not_found", "Endpoint not found")
	}
	jobID, action, outputID := m[1], m[2], m[3]
	job, err := rt.Owned(jobID, namespace)
	if err != nil {
		return err
	}
	switch {
	case action == "" && method == "GET":
		return q.send(200, PublicJob(job))
	case action == "cancel" && method == "POST":
		status := 202
		if c.Terminal(job.Str("state")) {
			status = 200
		}
		cancelled, err := rt.Cancel(jobID, namespace)
		if err != nil {
			return err
		}
		return q.send(status, PublicJob(cancelled))
	case action == "manifest" && method == "GET":
		if !job.Has("manifest") {
			return c.Err(409, "manifest_not_ready", "Manifest is available after durable output publication")
		}
		return q.send(200, job.Get("manifest"))
	case action == "outputs" && method == "GET" && outputID != "":
		return s.output(q, job, outputID)
	case action == "events" && method == "GET":
		return s.events(q, jobID)
	}
	return c.Err(404, "not_found", "Endpoint not found")
}

func (s *Server) modelDetail(q *request, modelID string, verify bool) error {
	rt := s.Runtime
	model := rt.Store.Registry("models", modelID)
	if model == nil {
		return c.Err(404, "not_found", "Model not found")
	}
	bundle := model.Obj("bundle")
	files := bundle.Obj("configuration").Obj("files")
	components := []any{}
	for _, raw := range bundle.Arr("components") {
		component := raw.(*jsonv.Object)
		count := 0
		for _, f := range files.Keys() {
			if strings.HasPrefix(f, component.Str("role")+"/") {
				count++
			}
		}
		components = append(components, jsonv.O(
			"role", component.Get("role"),
			"digest", component.Get("digest"),
			"tensor_format", component.Get("tensor_format"),
			"files", float64(count),
		))
	}
	detail := jsonv.O(
		"model_id", model.Get("model_id"),
		"artifact_handle", model.Get("artifact_handle"),
		"family", bundle.Get("family"),
		"bundle_digest", bundle.Get("bundle_digest"),
		"source", jsonv.Maybe(bundle.Get("source")),
		"components", components,
		"file_count", float64(files.Len()),
	)
	if verify {
		// Re-verify every byte through the worker and report the load estimate.
		if err := q.requireOperator(); err != nil {
			return err
		}
		p, err := rt.Worker.Probe()
		if err != nil {
			return err
		}
		result, err := rt.Worker.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "bundle", bundle, "options", jsonv.Maybe(p.Caps.Get("load_defaults"))))
		if err != nil {
			var api *c.ApiError
			if !errors.As(err, &api) || api.Status >= 500 {
				return err
			}
			detail.Set("verification", jsonv.O("valid", false, "error", api.Code))
		} else {
			detail.Set("verification", jsonv.O(
				"valid", true,
				"memory_estimate", nullish(result.Get("memory_estimate")),
				"load_defaults", jsonv.Maybe(p.Caps.Get("load_defaults")),
			))
		}
	}
	return q.send(200, detail)
}

func (s *Server) output(q *request, job *jsonv.Object, outputID string) error {
	var output *jsonv.Object
	for _, raw := range job.Arr("outputs") {
		if o, ok := raw.(*jsonv.Object); ok && o.Str("output_id") == outputID {
			output = o
			break
		}
	}
	if output == nil {
		return c.Err(404, "not_found", "Output not found")
	}
	file := filepath.Join(s.Runtime.OutputDirectory(job.Str("job_id")), outputID+".png")
	info, err := os.Stat(file)
	if c.Expired(output.Get("expires_at")) || err != nil {
		return c.Err(410, "output_expired", "Output is no longer on local storage")
	}
	f, err := os.Open(file)
	if err != nil {
		return c.Err(410, "output_expired", "Output is no longer on local storage")
	}
	defer f.Close()
	h := q.w.Header()
	h.Set("Content-Type", output.Str("mime_type"))
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("ETag", `"`+output.Str("sha256")+`"`)
	q.w.WriteHeader(200)
	q.sent = true
	if _, err := io.Copy(q.w, f); err != nil {
		return err
	}
	return nil
}

func (s *Server) events(q *request, jobID string) error {
	rt := s.Runtime
	lastID, _ := header(q.r, "Last-Event-ID")
	cursor, ok := jsNumber(lastID, 0)
	events := rt.Store.Events(jobID, 0)
	var last float64
	if len(events) > 0 {
		last, _ = events[len(events)-1].Num("sequence")
	}
	if !ok || cursor < 0 || cursor > last {
		return c.Err(409, "resume_required", "Fetch a fresh job snapshot before resuming events")
	}
	// The request has been read; never let ReadTimeout bound a long-lived stream.
	_ = http.NewResponseController(q.w).SetReadDeadline(time.Time{})
	h := q.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	q.w.WriteHeader(200)
	q.sent = true
	flusher, _ := q.w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var out strings.Builder
		for _, event := range rt.Store.Events(jobID, int64(cursor)) {
			sequence, _ := event.Num("sequence")
			fmt.Fprintf(&out, "id: %s\nevent: %s\ndata: %s\n\n", jsonv.Number(sequence), event.Str("type"), jsonv.MustMarshal(event))
			cursor = sequence
			if out.Len() > 1024*1024 {
				return errors.New("slow event consumer")
			}
		}
		current := rt.Store.Get(jobID, "")
		done := current == nil || c.Terminal(current.Str("state"))
		if !done {
			out.WriteString(": heartbeat\n\n")
		}
		if _, err := io.WriteString(q.w, out.String()); err != nil {
			return err
		}
		flush()
		if done {
			return nil
		}
		select {
		case <-ticker.C:
		case <-q.r.Context().Done():
			return nil
		case <-s.closing:
			return nil
		}
	}
}

// jsNumber is Number(value || fallback) as the TypeScript API parsed query parameters and
// Last-Event-ID; ok is Number.isInteger of the result.
func jsNumber(value string, fallback float64) (float64, bool) {
	if value == "" {
		return fallback, true
	}
	t := strings.Trim(value, " \t\n\v\f\r\u00a0\ufeff\u2028\u2029")
	var f float64
	switch {
	case t == "":
		return 0, true
	case decimal.MatchString(t):
		f, _ = strconv.ParseFloat(t, 64)
	case radix.MatchString(t):
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[t[1]|0x20]
		n, err := strconv.ParseUint(t[2:], base, 64)
		if err != nil {
			return 0, false
		}
		f = float64(n)
	default:
		return 0, false
	}
	return f, !math.IsInf(f, 0) && f == math.Trunc(f)
}

var (
	decimal = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	radix   = regexp.MustCompile(`^0([xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
)

func protocols() []any {
	out := make([]any, len(c.PROTOCOLS))
	for i, p := range c.PROTOCOLS {
		out[i] = p
	}
	return out
}

func nullish(v any) any { return v }

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

// PublicInput omits the owning namespace.
func PublicInput(record *jsonv.Object) *jsonv.Object {
	out := record.Clone()
	out.Delete("namespace")
	return out
}

// PublicJob is a job without its namespace, registry snapshots or manifest.
func PublicJob(job *jsonv.Object) *jsonv.Object {
	out := jsonv.NewObject()
	for _, k := range job.Keys() {
		switch k {
		case "namespace", "model", "backend", "recipe", "manifest", "input", "adapters":
			continue
		}
		out.Set(k, job.Get(k))
	}
	if input := job.Obj("input"); input != nil {
		out.Set("input", PublicInput(input))
	}
	adapters := []any{}
	for _, raw := range job.Arr("adapters") {
		a := raw.(*jsonv.Object)
		adapter := a.Obj("adapter")
		adapters = append(adapters, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "scale", a.Get("scale")))
	}
	out.Set("adapters", adapters)
	model := job.Obj("model")
	out.Set("model", jsonv.O("model_id", model.Get("model_id"), "bundle_digest", model.Obj("bundle").Get("bundle_digest")))
	recipe := job.Obj("recipe")
	out.Set("recipe", jsonv.O("recipe_id", recipe.Get("recipe_id"), "revision", recipe.Get("revision"), "digest", recipe.Get("digest")))
	out.Set("backend", jsonv.Maybe(job.Get("backend")))
	return out
}
