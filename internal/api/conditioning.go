package api

import (
	"github.com/jxburros/media-runtime/internal/catalog"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func (s *Server) conditioning(q *request, path, method string) (bool, error) {
	if path != "/v1/conditioning" {
		return false, nil
	}
	switch {
	case path == "/v1/conditioning" && method == "GET":
		entries := []any{}
		for _, component := range s.Runtime.Store.RegistryAll("conditioning") {
			entries = append(entries, conditioningSummary(component))
		}
		return true, q.send(200, entries)
	case path == "/v1/conditioning" && method == "POST":
		if err := q.requireOperator(); err != nil {
			return true, err
		}
		body, err := q.body()
		if err != nil {
			return true, err
		}
		component, err := catalog.BuildConditioning(body, s.Runtime.Config.CatalogSnapshot())
		if err != nil {
			return true, err
		}

		use := jsonv.O("component", component)
		switch component.Str("kind") {
		case "textual-inversion":
			use.Set("token", "<runtime-validation>").Set("encoder", "text_encoder")
		case "ip-adapter":
			use.Set("scale", 1.0)
		}
		validated := false
		for _, member := range s.Runtime.Pool.Members {
			probe, err := s.Runtime.Pool.Probe(member)
			if err != nil || c.Minor(probe.Protocol) < 4 || probe.Caps.Obj("conditioning") == nil {
				continue
			}
			if _, err := member.Client.Obj("/v1/validate", "POST", jsonv.O("protocol", probe.Protocol, "conditioning", []any{use})); err != nil {
				return true, err
			}
			validated = true
			break
		}
		if !validated {
			return true, c.Err(503, "unsupported", "No worker advertises conditioning validation")
		}
		s.Runtime.Store.Register("conditioning", component.Str("conditioning_id"), component)
		s.Runtime.Security.Audit.Record("conditioning.register", q.keyID, component.Str("conditioning_id"))
		return true, q.send(201, conditioningSummary(component))
	}
	return true, c.Err(404, "not_found", "Endpoint not found")
}

func publicConditioning(op *jsonv.Object) []any {
	out := []any{}
	for _, raw := range op.Arr("conditioning") {
		use := raw.(*jsonv.Object)
		component := use.Obj("component")
		entry := jsonv.O("conditioning_id", component.Get("conditioning_id"), "kind", component.Get("kind"), "digest", component.Get("digest"))
		for _, name := range []string{"scale", "token", "encoder"} {
			if use.Has(name) {
				entry.Set(name, use.Get(name))
			}
		}
		out = append(out, entry)
	}
	return out
}
func publicControlnets(op *jsonv.Object) []any {
	out := []any{}
	for _, raw := range op.Arr("controlnets") {
		use := raw.(*jsonv.Object)
		component := use.Obj("controlnet")
		out = append(out, jsonv.O("controlnet_id", component.Get("controlnet_id"), "digest", component.Get("digest"), "scale", use.Get("scale")))
	}
	return out
}

func conditioningSummary(component *jsonv.Object) *jsonv.Object {
	return jsonv.O("conditioning_id", component.Get("conditioning_id"), "kind", component.Get("kind"), "digest", component.Get("digest"), "source", component.Get("source"))
}
