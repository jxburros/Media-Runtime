package contracts

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"regexp"
)

var ConditioningRegistration = StrictObject(
	Field{Name: "conditioning_id", Check: ID},
	Field{Name: "kind", Check: Enum("vae", "textual-inversion", "ip-adapter")},
	Field{Name: "artifact_handle", Check: ID},
	Field{Name: "source", Check: sourceSchema, Default: func() any { return jsonv.NewObject() }},
)

var ConditioningUse = StrictObject(
	Field{Name: "conditioning_id", Check: ID},
	Field{Name: "scale", Check: Num(NumOpts{Min: F(0), Max: F(2)}), Optional: true},
	Field{Name: "token", Check: Str(StrOpts{Min: 1, Max: 80, Pattern: regexp.MustCompile(`^\S+$`)}), Optional: true},
	Field{Name: "encoder", Check: Enum("text_encoder", "text_encoder_2"), Optional: true},
)
var ConditioningUses = Array(ConditioningUse, ArrOpts{Max: 4})
