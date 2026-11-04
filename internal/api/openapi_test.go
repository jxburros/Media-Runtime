package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Check direct route predicates against the same inventory used by OpenAPI.
// Parameter routes are backed by the named regexp/prefix dispatches below.
func TestRouteDocumentationDoesNotDrift(t *testing.T) {
	sources := []string{"api.go", "security.go", "operations.go"}
	seen := map[string]bool{}
	for _, name := range sources {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "route" && fn.Name.Name != "securityRoutes" && fn.Name.Name != "operations") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var expressions []ast.Expr
				switch v := n.(type) {
				case *ast.IfStmt:
					expressions = []ast.Expr{v.Cond}
				case *ast.CaseClause:
					expressions = v.List
				default:
					return true
				}
				for _, expr := range expressions {
					paths, methods := []string{}, []string{}
					ast.Inspect(expr, func(child ast.Node) bool {
						b, ok := child.(*ast.BinaryExpr)
						if !ok || b.Op != token.EQL {
							return true
						}
						id, ok := b.X.(*ast.Ident)
						if !ok {
							return true
						}
						lit, ok := b.Y.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							return true
						}
						value, _ := strconv.Unquote(lit.Value)
						if id.Name == "path" {
							paths = append(paths, value)
						}
						if id.Name == "method" {
							methods = append(methods, value)
						}
						return true
					})
					for _, path := range paths {
						for _, method := range methods {
							found := false
							for _, documented := range RouteDescriptions[path] {
								if documented == method {
									found = true
								}
							}
							if !found {
								t.Errorf("served route missing from OpenAPI: %s %s", method, path)
							}
							seen[path] = true
						}
					}
				}
				return true
			})
		}
	}
	for path := range RouteDescriptions {
		if strings.Contains(path, "{") {
			continue
		}
		if !seen[path] {
			t.Errorf("documented path has no served route predicate: %s", path)
		}
	}
	parameterSources := map[string]string{"/v1/inputs/{input_id}": "inputPath", "/v1/models/{model_id}": "modelPath", "/v1/recipes/{recipe_id}": "recipePath", "/v1/jobs/{job_id}": "jobPath", "/v1/jobs/{job_id}/events": "jobPath", "/v1/jobs/{job_id}/cancel": "jobPath", "/v1/jobs/{job_id}/manifest": "jobPath", "/v1/jobs/{job_id}/outputs/{output_id}": "jobPath", "/v1/jobs/{job_id}/previews/{preview_id}": "jobPath", "/v1/keys/{key_id}": "/v1/keys/", "/v1/folders/{folder_id}": "/v1/folders/"}
	combined := []byte{}
	for _, name := range sources {
		b, _ := os.ReadFile(name)
		combined = append(combined, b...)
	}
	for path, symbol := range parameterSources {
		if _, ok := RouteDescriptions[path]; !ok || !bytes.Contains(combined, []byte(symbol)) {
			t.Errorf("parameter route drift: %s", path)
		}
	}
}

func TestCommittedOpenAPI(t *testing.T) {
	filename := filepath.Join("..", "..", "docs", "openapi.json")
	expected := append(jsonv.Indent(OpenAPI()), '\n')
	if os.Getenv("UPDATE_OPENAPI") == "1" {
		if err := os.WriteFile(filename, expected, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("docs/openapi.json drifted; run go run ./cmd/openapi > docs/openapi.json")
	}
}
