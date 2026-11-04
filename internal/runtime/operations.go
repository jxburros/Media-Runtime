package runtime

import (
	"context"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/operations"
	"time"
)

func (r *Runtime) startOperations() {
	ctx, cancel := context.WithCancel(context.Background())
	r.webhookCancel = cancel
	r.loop.Add(1)
	go func() {
		defer r.loop.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		scans := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scans++
				if scans%30 == 0 {
					_ = r.RefreshFolders()
				}
				if len(r.Config.Webhooks) == 0 {
					continue
				}
				for _, job := range r.Store.All("") {
					if c.Terminal(job.Str("state")) {
						_ = r.Webhooks.Deliver(ctx, operations.CompletionEvent(job.Str("job_id"), job.Str("state"), job.Str("updated_at")))
					}
				}
			}
		}
	}()
}
