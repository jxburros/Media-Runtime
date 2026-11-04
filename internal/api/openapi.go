package api

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"sort"
)

// RouteDescriptions is the public route inventory used to generate the committed
// OpenAPI document. The drift test checks it against the route source files.
var RouteDescriptions = map[string][]string{
	"/health":                                 {"GET"},
	"/.well-known/media-runtime":              {"GET"},
	"/v1/runtime":                             {"GET"},
	"/v1/workers":                             {"GET"},
	"/v1/catalog":                             {"GET"},
	"/v1/inputs":                              {"GET", "POST"},
	"/v1/inputs/{input_id}":                   {"GET", "DELETE"},
	"/v1/models":                              {"GET", "POST"},
	"/v1/models/{model_id}":                   {"GET"},
	"/v1/adapters":                            {"GET", "POST"},
	"/v1/recipes":                             {"GET", "POST"},
	"/v1/recipes/{recipe_id}":                 {"GET"},
	"/v1/jobs":                                {"GET", "POST"},
	"/v1/jobs/validate":                       {"POST"},
	"/v1/jobs/{job_id}":                       {"GET"},
	"/v1/jobs/{job_id}/events":                {"GET"},
	"/v1/jobs/{job_id}/cancel":                {"POST"},
	"/v1/jobs/{job_id}/manifest":              {"GET"},
	"/v1/jobs/{job_id}/outputs/{output_id}":   {"GET"},
	"/v1/jobs/{job_id}/previews/{preview_id}": {"GET"},
	"/v1/keys":                                {"GET", "POST"},
	"/v1/keys/{key_id}":                       {"DELETE"},
	"/v1/pairing":                             {"POST"},
	"/v1/pairing/redeem":                      {"POST"},
	"/v1/diagnostics":                         {"GET"},
	"/v1/openapi.json":                        {"GET"},
	"/metrics":                                {"GET"},
	"/v1/folders":                             {"GET", "POST"},
	"/v1/folders/{folder_id}":                 {"DELETE"},
}

func OpenAPI() *jsonv.Object {
	paths := jsonv.NewObject()
	ordered := make([]string, 0, len(RouteDescriptions))
	for path := range RouteDescriptions {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		methods := RouteDescriptions[path]
		operations := jsonv.NewObject()
		for _, method := range methods {
			lower := map[string]string{"GET": "get", "POST": "post", "DELETE": "delete"}[method]
			public := path == "/health" || path == "/.well-known/media-runtime" || path == "/v1/pairing/redeem"
			auth := []any{jsonv.O("bearerAuth", []any{})}
			if public {
				auth = []any{}
			}
			operation := jsonv.O("summary", method+" "+path, "security", auth, "responses", jsonv.O("2XX", jsonv.O("description", "Successful response"), "4XX", jsonv.O("description", "Request refused", "content", jsonv.O("application/json", jsonv.O("schema", jsonv.O("$ref", "#/components/schemas/Error"))))))
			params := []any{}
			for _, name := range []string{"input_id", "model_id", "recipe_id", "job_id", "output_id", "key_id", "folder_id", "preview_id"} {
				if containsPathParameter(path, name) {
					params = append(params, jsonv.O("name", name, "in", "path", "required", true, "schema", jsonv.O("type", "string")))
				}
			}
			if len(params) > 0 {
				operation.Set("parameters", params)
			}
			if method == "POST" {
				content := "application/json"
				schema := jsonv.O("type", "object")
				if path == "/v1/inputs" {
					content = "image/png"
					schema = jsonv.O("type", "string", "format", "binary")
				}
				operation.Set("requestBody", jsonv.O("required", path != "/v1/jobs/{job_id}/cancel", "content", jsonv.O(content, jsonv.O("schema", schema))))
			}
			operations.Set(lower, operation)
		}
		paths.Set(path, operations)
	}
	return jsonv.O("openapi", "3.0.3", "info", jsonv.O("title", "Media Runtime", "version", c.VERSION), "paths", paths,
		"components", jsonv.O("securitySchemes", jsonv.O("bearerAuth", jsonv.O("type", "http", "scheme", "bearer")), "schemas", jsonv.O("Error", jsonv.O("type", "object", "required", []any{"error", "request_id"}, "properties", jsonv.O("request_id", jsonv.O("type", "string"), "error", jsonv.O("type", "object", "properties", jsonv.O("code", jsonv.O("type", "string"), "type", jsonv.O("type", "string"), "message", jsonv.O("type", "string"), "field", jsonv.O("type", "string"), "repair", jsonv.O("type", "object"))))))))
}

func containsPathParameter(path, name string) bool {
	needle := "{" + name + "}"
	for i := 0; i+len(needle) <= len(path); i++ {
		if path[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
