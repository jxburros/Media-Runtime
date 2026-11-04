package runtime

import (
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// CheckModels applies a key's model scope to every operation before resolution.
func (caller Caller) CheckModels(body any) error {
	if len(caller.Models) == 0 {
		return nil
	}
	object, ok := body.(*jsonv.Object)
	if !ok {
		return nil
	}
	check := func(id string) error {
		if id != "" && !c.Contains(caller.Models, id) {
			return c.Err(403, "model_forbidden", "Credential cannot use this model", "model_id")
		}
		return nil
	}
	if err := check(object.Str("model_id")); err != nil {
		return err
	}
	if models := object.Obj("models"); models != nil {
		for _, key := range models.Keys() {
			if err := check(models.Str(key)); err != nil {
				return err
			}
		}
	}
	return nil
}
