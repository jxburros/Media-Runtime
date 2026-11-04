package runtime

import (
	"errors"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"os"
)

func (r *Runtime) Cleanup() error {
	if err := r.RefreshFolders(); err != nil {
		return err
	}
	for _, job := range r.Store.All("") {
		if !c.Terminal(job.Str("state")) {
			continue
		}
		records := append(append([]any{}, job.Arr("outputs")...), job.Arr("intermediate_outputs")...)
		for _, op := range Operations(job) {
			records = append(records, op.Arr("outputs")...)
		}
		removed := map[string]bool{}
		for _, raw := range records {
			output, _ := raw.(*jsonv.Object)
			path := r.OutputFile(job.Str("job_id"), output)
			if c.Expired(output.Get("expires_at")) && !removed[path] {
				removed[path] = true
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	for _, input := range r.Store.Inputs("") {
		if c.Expired(input.Get("expires_at")) && !r.referenced(input.Str("input_id")) {
			if err := os.Remove(r.InputPath(input.Str("input_id"))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
