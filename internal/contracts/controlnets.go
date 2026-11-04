package contracts

import "github.com/jxburros/media-runtime/internal/jsonv"

var ControlNetUse = StrictObject(
	Field{Name: "controlnet_id", Check: ID},
	Field{Name: "scale", Check: Num(NumOpts{Min: F(0), Max: F(2)}), Default: func() any { return 1.0 }},
)
var controlnetList = Array(ControlNetUse, ArrOpts{Max: 1})
var ControlNetRegistration = StrictObject(
	Field{Name: "controlnet_id", Check: ID},
	Field{Name: "artifact_handle", Check: ID},
	Field{Name: "source", Check: sourceSchema, Default: func() any { return jsonv.NewObject() }},
)
