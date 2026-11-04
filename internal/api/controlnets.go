package api

import (
	"github.com/jxburros/media-runtime/internal/catalog"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func (s *Server) controlnets(q *request, path, method string) (bool, error) {
	if path != "/v1/controlnets" {
		return false, nil
	}
	rt := s.Runtime
	if path == "/v1/controlnets" && method == "GET" {
		list := []any{}
		for _, d := range rt.Store.RegistryAll("controlnets") {
			list = append(list, jsonv.O("controlnet_id", d.Get("controlnet_id"), "digest", d.Get("digest"), "family", d.Get("family"), "source", d.Get("source")))
		}
		return true, q.send(200, list)
	}
	if method != "POST" {
		return false, nil
	}
	if path == "/v1/controlnets" && method == "POST" {
		if err := q.requireOperator(); err != nil {
			return true, err
		}
		raw, err := q.body()
		if err != nil {
			return true, err
		}
		d, err := catalog.BuildControlNet(raw, rt.Config.CatalogSnapshot())
		if err != nil {
			return true, err
		}
		for _, member := range rt.Pool.Members {
			p, err := rt.Pool.Probe(member)
			if err != nil || c.Minor(p.Protocol) < 4 || p.Caps.Obj("controlnets") == nil {
				continue
			}
			_, err = member.Client.Obj("/v1/validate", "POST", jsonv.O("protocol", p.Protocol, "controlnets", []any{jsonv.O("controlnet", d, "scale", 1.0)}))
			if err != nil {
				return true, err
			}
			rt.Store.Register("controlnets", d.Str("controlnet_id"), d)
			rt.Security.Audit.Record("controlnet.registered", q.keyID, d.Str("controlnet_id"))
			return true, q.send(201, jsonv.O("controlnet_id", d.Get("controlnet_id"), "digest", d.Get("digest")))
		}
		return true, c.Err(503, "unsupported", "No worker advertises ControlNet validation")
	}
	return false, nil
}
