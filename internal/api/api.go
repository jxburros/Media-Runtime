// Package api is the authenticated HTTP API and its server-sent event streams.
package api

import (
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/media-runtime/internal/catalog"
	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/runtime"
	"github.com/jxburros/media-runtime/internal/transport"
)

// Server is the runtime's HTTP service. Close ends open event streams before shutting down.
type Server struct {
	Runtime *runtime.Runtime
	http    *http.Server
	ln      net.Listener
	closing chan struct{}
	once    sync.Once
	// scheme is "https" when the listener serves TLS; fingerprint is its certificate's SHA-256.
	scheme      string
	host        string
	fingerprint string
}

func New(rt *runtime.Runtime) *Server { return NewWithTimeouts(rt, 10*time.Second, 30*time.Second) }

// NewWithTimeouts sets the header and whole-request read limits (tests shorten them).
func NewWithTimeouts(rt *runtime.Runtime, header, request time.Duration) *Server {
	s := &Server{Runtime: rt, closing: make(chan struct{})}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: header, ReadTimeout: request}
	return s
}

// Listen binds a loopback address (port 0 picks a free port) and serves plain HTTP in the
// background; it refuses any other address. Serve is the configured listener.
func (s *Server) Listen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if err := config.CheckBind(host, nil); err != nil {
		return err
	}
	return s.listen(addr, nil)
}

// Serve listens on the configured bind and port: HTTPS with the configured certificate when tls
// is set, plain HTTP only on loopback. A non-loopback bind without tls is refused before any
// socket is opened.
func (s *Server) Serve(bind string, port int, material *config.ServerTLS) error {
	if bind == "" {
		bind = config.DefaultBind
	}
	if err := config.CheckBind(bind, material); err != nil {
		return err
	}
	var tc *tls.Config
	if material != nil {
		var err error
		if tc, s.fingerprint, err = transport.Server(material.CertFile, material.KeyFile); err != nil {
			return err
		}
	}
	return s.listen(net.JoinHostPort(bind, fmt.Sprint(port)), tc)
}

func (s *Server) listen(addr string, tc *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.scheme, s.host = "http", "127.0.0.1"
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsUnspecified() {
		s.host = ip.String()
	}
	if tc != nil {
		s.scheme = "https"
		ln = tls.NewListener(ln, tc)
	}
	s.ln = ln
	go func() { _ = s.http.Serve(ln) }()
	return nil
}

func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// URL is where a client on this host reaches the API (loopback for a wildcard bind).
func (s *Server) URL() string {
	return s.scheme + "://" + net.JoinHostPort(s.host, fmt.Sprint(s.Port()))
}

// Scheme is "http" or "https".
func (s *Server) Scheme() string { return s.scheme }

// Fingerprint is the SHA-256 of the API certificate's DER, or "" without TLS.
func (s *Server) Fingerprint() string { return s.fingerprint }

// Close ends event streams and closes every connection.
func (s *Server) Close() error {
	s.once.Do(func() { close(s.closing) })
	return s.http.Close()
}

var (
	namespacePattern = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,200}$`)
	inputPath        = regexp.MustCompile(`^/v1/inputs/(input_[a-f0-9]{32})$`)
	modelPath        = regexp.MustCompile(`^/v1/models/([a-zA-Z0-9_-]{1,80})$`)
	recipePath       = regexp.MustCompile(`^/v1/recipes/([a-zA-Z0-9_-]{1,80})$`)
	workerPattern    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
	// Output IDs are the worker's (image, image_2 …) or, for a linear job's intermediates and
	// colliding published outputs, <op_id>__<output_id>.
	jobPath = regexp.MustCompile(`^/v1/jobs/(job_[a-f0-9-]+)(?:/(events|cancel|manifest|outputs)(?:/([a-z][a-z0-9_]{0,65}))?)?$`)
)

type request struct {
	w         http.ResponseWriter
	r         *http.Request
	sent      bool
	namespace string
	operator  bool
	fallbacks bool
}

func (q *request) caller() runtime.Caller {
	return runtime.Caller{Namespace: q.namespace, AllowFallbacks: q.fallbacks}
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

// allowedOrigin matches a request's Origin exactly (scheme, host and port) against the
// configured allow-list; an empty list refuses every browser origin.
func allowedOrigin(allowed []string, origin string) bool {
	normalized, ok := config.NormalizeOrigin(origin)
	if !ok || normalized != strings.ToLower(origin) {
		return false
	}
	for _, a := range allowed {
		if a == normalized {
			return true
		}
	}
	return false
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
	if origin, ok := header(r, "Origin"); ok {
		if !allowedOrigin(cfg.AllowedOrigins, origin) {
			return c.Err(403, "browser_origin_denied", "Use an authenticated application gateway")
		}
		// An allow-listed origin may read responses; it still needs a bearer credential.
		h := q.w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Expose-Headers", "Retry-After")
		if method == "OPTIONS" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Media-Namespace, Idempotency-Key, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			q.w.WriteHeader(204)
			q.sent = true
			return nil
		}
	}
	if path == "/health" && method == "GET" {
		return q.send(200, jsonv.O("status", "ok"))
	}
	supplied, _ := header(r, "Authorization")
	var credential *config.Token
	for i, t := range cfg.Tokens {
		expected := "Bearer " + t.Token
		if len(expected) == len(supplied) && subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1 {
			credential = &cfg.Tokens[i]
			break
		}
	}
	if credential == nil {
		return c.Err(401, "unauthorized", "A valid bearer credential is required")
	}
	q.namespace, q.operator, q.fallbacks = credential.Namespace, credential.Operator, credential.AllowFallbacks
	if suppliedNamespace, ok := header(r, "X-Media-Namespace"); ok {
		if !namespacePattern.MatchString(suppliedNamespace) {
			return c.Err(400, "invalid_namespace", "Invalid caller namespace")
		}
		allowed := false
		for _, n := range credential.AllowedNamespaces {
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
				"max_output_bytes", outputLimits(cfg),
			)).
			Set("protocols", protocols()))
	case path == "/v1/workers" && method == "GET":
		return q.send(200, rt.Workers(namespace))
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
			models = append(models, withFacts(jsonv.O(
				"model_id", model.Get("model_id"),
				"family", bundle.Get("family"),
				"tasks", familyTasks(bundle.Str("family")),
				"scale", jsonv.Maybe(model.Get("scale")),
				"bundle_digest", bundle.Get("bundle_digest"),
				"source", jsonv.Maybe(bundle.Get("source")),
				"available", statErr == nil && cfg.Catalog(model.Str("artifact_handle")) != "",
			), model))
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
		// Registration validates through the first available worker of the pool.
		w, p, err := rt.Pool.First()
		if err != nil {
			return err
		}
		family := model.Str("family")
		if family != "sdxl-base" && (c.Minor(p.Protocol) < c.FamilyMinor(family) || !workerLoads(p.Caps, family)) {
			return c.Err(503, "unsupported", "The connected worker does not load the "+family+" family", "family")
		}
		validation, err := w.Client.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "bundle", model.Get("bundle"), "options", jsonv.Maybe(p.Caps.Get("load_defaults"))))
		if err != nil {
			return err
		}
		if scale, ok := validation.Num("scale"); ok && family == "esrgan" {
			// The worker read the network configuration; its factor is the one recorded.
			model.Set("scale", scale)
		}
		// The bundle facts the worker read from the configurations (video sizes and default
		// frame count for svd) are recorded with the model, as the upscale factor is.
		for _, k := range c.BundleFacts[family] {
			if v := validation.Get(k); v != nil {
				model.Set(k, v)
			}
		}
		rt.Store.Register("models", model.Str("model_id"), model)
		return q.send(201, withFacts(jsonv.O(
			"model_id", model.Get("model_id"),
			"family", model.Get("family"),
			"scale", jsonv.Maybe(model.Get("scale")),
			"bundle_digest", model.Obj("bundle").Get("bundle_digest"),
			"conversion", jsonv.Maybe(model.Obj("bundle").Obj("source").Get("conversion")),
			"memory_estimate", nullish(validation.Get("memory_estimate")),
		), model))
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
		w, p, err := rt.Pool.First()
		if err != nil {
			return err
		}
		if !truthy(p.Caps.Get("adapters")) || p.Protocol == c.PROTOCOLS[0] {
			return c.Err(503, "unsupported", "The connected worker does not advertise LoRA adapters")
		}
		validation, err := w.Client.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "adapters", []any{jsonv.O("adapter", adapter, "scale", 1.0)}))
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
	case recipePath.MatchString(path) && method == "GET":
		recipe := rt.Store.Registry("recipes", recipePath.FindStringSubmatch(path)[1])
		if recipe == nil {
			return c.Err(404, "not_found", "Recipe not found")
		}
		return q.send(200, RecipeDetail(recipe))
	case path == "/v1/jobs/validate" && method == "POST":
		body, err := q.body()
		if err != nil {
			return err
		}
		result, err := rt.Resolve(body, q.caller())
		if err != nil {
			return err
		}
		candidates := []any{}
		for _, raw := range result.Arr("candidates") {
			candidates = append(candidates, raw.(*jsonv.Object).Get("worker_id"))
		}
		response := jsonv.O(
			"valid", true,
			"request", result.Get("request"),
			"recipe_digest", result.Obj("recipe").Get("digest"),
			"bundle_digest", result.Obj("model").Obj("bundle").Get("bundle_digest"),
			"candidate_workers", candidates,
		)
		if c.IsLinear(result.Obj("recipe")) {
			// The per-operation plan: each operation's model, task and effective parameters.
			plan := []any{}
			for _, op := range runtime.Operations(result) {
				plan = append(plan, jsonv.O(
					"op_id", op.Get("op_id"),
					"task", op.Get("task"),
					"model_id", op.Obj("model").Get("model_id"),
					"bundle_digest", op.Obj("model").Obj("bundle").Get("bundle_digest"),
					"input_from", jsonv.Maybe(op.Get("input_from")),
					"parameters", op.Get("parameters"),
				))
			}
			response.Set("operations", plan)
		}
		return q.send(200, response)
	case path == "/v1/jobs" && method == "POST":
		var key *string
		if k, ok := header(r, "Idempotency-Key"); ok {
			key = &k
		}
		body, err := q.body()
		if err != nil {
			return err
		}
		job, err := rt.Submit(body, q.caller(), key)
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
		workerID := query.Get("worker_id")
		if query.Has("worker_id") && !workerPattern.MatchString(workerID) {
			return c.Err(400, "invalid_request", "worker_id must be a worker ID", "worker_id")
		}
		jobs, ordinals := rt.Store.Page(namespace, newest, int(after), int(limit), workerID)
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
		"tasks", familyTasks(bundle.Str("family")),
		"scale", jsonv.Maybe(model.Get("scale")),
		"bundle_digest", bundle.Get("bundle_digest"),
		"source", jsonv.Maybe(bundle.Get("source")),
		"components", components,
		"file_count", float64(files.Len()),
	)
	withFacts(detail, model)
	if verify {
		// Re-verify every byte through the worker and report the load estimate.
		if err := q.requireOperator(); err != nil {
			return err
		}
		w, p, err := rt.Pool.First()
		if err != nil {
			return err
		}
		result, err := w.Client.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "bundle", bundle, "options", jsonv.Maybe(p.Caps.Get("load_defaults"))))
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
	// The job owns its intermediates, so they are served beside the published outputs.
	for _, raw := range append(append([]any{}, job.Arr("outputs")...), job.Arr("intermediate_outputs")...) {
		if o, ok := raw.(*jsonv.Object); ok && o.Str("output_id") == outputID {
			output = o
			break
		}
	}
	if output == nil {
		return c.Err(404, "not_found", "Output not found")
	}
	file := s.Runtime.OutputFile(job.Str("job_id"), output)
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

func outputLimits(cfg *config.Config) *jsonv.Object {
	limits := jsonv.NewObject()
	for _, mime := range config.OutputMIMEs {
		limits.Set(mime, float64(cfg.OutputLimit(mime)))
	}
	return limits
}

// withFacts adds a model's recorded bundle facts (video sizes, default frames, audio facts).
func withFacts(o, model *jsonv.Object) *jsonv.Object {
	for _, family := range c.FAMILY_NAMES {
		for _, k := range c.BundleFacts[family] {
			if k != "scale" && model.Has(k) {
				o.Set(k, model.Get(k))
			}
		}
	}
	return o
}

func familyTasks(family string) []any {
	tasks := []any{}
	for _, t := range c.FAMILIES[family] {
		tasks = append(tasks, t)
	}
	return tasks
}

func workerLoads(caps *jsonv.Object, family string) bool {
	for _, raw := range caps.Arr("loaders") {
		if l, ok := raw.(*jsonv.Object); ok {
			for _, f := range l.Arr("families") {
				if f == family {
					return true
				}
			}
		}
	}
	return false
}

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

// publicOperation is an operation without its registry snapshots or input owner.
func publicOperation(op *jsonv.Object) *jsonv.Object {
	out := jsonv.NewObject()
	for _, k := range op.Keys() {
		switch k {
		case "model":
			model := op.Obj("model")
			out.Set(k, jsonv.O("model_id", model.Get("model_id"), "bundle_digest", model.Obj("bundle").Get("bundle_digest")))
		case "input":
			if input := op.Obj("input"); input != nil {
				out.Set(k, PublicInput(input))
			}
		case "adapters":
			out.Set(k, publicAdapters(op))
		default:
			out.Set(k, op.Get(k))
		}
	}
	return out
}

func publicAdapters(holder *jsonv.Object) []any {
	adapters := []any{}
	for _, raw := range holder.Arr("adapters") {
		a := raw.(*jsonv.Object)
		adapter := a.Obj("adapter")
		adapters = append(adapters, jsonv.O("adapter_id", adapter.Get("adapter_id"), "digest", adapter.Get("digest"), "scale", a.Get("scale")))
	}
	return adapters
}

// RecipeDetail is a recipe as GET /v1/recipes/{id} returns it: its registered identity and
// schema version with the typed input declarations, operations, bindings and publish list of
// its schema-2 form, which Studio's native-workflow path maps inputs from.
func RecipeDetail(recipe *jsonv.Object) *jsonv.Object {
	normalized := c.NormalizeRecipe(recipe)
	return jsonv.O(
		"recipe_id", recipe.Get("recipe_id"),
		"revision", recipe.Get("revision"),
		"schema_version", recipe.Get("schema_version"),
		"digest", recipe.Get("digest"),
		"inputs", normalized.Get("inputs"),
		"operations", normalized.Get("operations"),
		"publish", normalized.Get("publish"),
	)
}

// PublicJob is a job without its namespace, registry snapshots or manifest.
func PublicJob(job *jsonv.Object) *jsonv.Object {
	out := jsonv.NewObject()
	for _, k := range job.Keys() {
		switch k {
		case "namespace", "model", "backend", "recipe", "manifest", "input", "adapters":
			continue
		case "operations":
			ops := []any{}
			for _, op := range runtime.Operations(job) {
				ops = append(ops, publicOperation(op))
			}
			out.Set(k, ops)
			continue
		}
		out.Set(k, job.Get(k))
	}
	if c.IsLinear(job.Obj("recipe")) {
		if ops := runtime.Operations(job); len(ops) > 0 {
			index, _ := job.Num("current_operation")
			out.Set("current_op_id", ops[min(int(index), len(ops)-1)].Get("op_id"))
		}
	}
	if input := job.Obj("input"); input != nil {
		out.Set("input", PublicInput(input))
	}
	out.Set("adapters", publicAdapters(job))
	model := job.Obj("model")
	out.Set("model", jsonv.O("model_id", model.Get("model_id"), "bundle_digest", model.Obj("bundle").Get("bundle_digest")))
	recipe := job.Obj("recipe")
	out.Set("recipe", jsonv.O("recipe_id", recipe.Get("recipe_id"), "revision", recipe.Get("revision"), "digest", recipe.Get("digest")))
	out.Set("backend", jsonv.Maybe(job.Get("backend")))
	return out
}
