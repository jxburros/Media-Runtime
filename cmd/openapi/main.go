// Command openapi prints the runtime API document; redirect to docs/openapi.json.
package main

import (
	"fmt"
	"github.com/jxburros/media-runtime/internal/api"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func main() { fmt.Println(string(jsonv.Indent(api.OpenAPI()))) }
