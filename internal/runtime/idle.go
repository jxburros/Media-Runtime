package runtime

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
	"time"
)

// maybeIdleUnload considers every lane, then lets only the oldest eligible idle
// lane unload. The caller owns that lane; no active worker is evicted to free GPU
// memory for another service. Operator-set memory budgets remain authoritative.
func (r *Runtime) maybeIdleUnload(member *pool.Member) {
	selected := func() *pool.View {
		r.mu.Lock()
		defer r.mu.Unlock()
		now := time.Now()
		var oldest *pool.View
		var oldestTime time.Time
		queued := r.Store.All("queued")
		jobs := r.Store.All("")
		for _, view := range r.Pool.Views() {
			id := view.Member.ID
			if view.Member.Config.IdleUnloadMinutes <= 0 || !view.Ready() {
				continue
			}
			busy := r.runningOn(id) != nil
			for _, job := range queued {
				if Eligible(job, Lane{ID: id, Probe: view.Probe, Idle: true}) {
					busy = true
					break
				}
			}
			var newest *jsonv.Object
			for _, job := range jobs {
				if job.Str("worker_id") == id && (newest == nil || job.Str("updated_at") > newest.Str("updated_at")) {
					newest = job
				}
			}
			if newest != nil && newest.Obj("request").Bool("keep_loaded") {
				busy = true
			}
			if busy || view.Probe.Info.Str("state") != "ready" || view.Probe.Info.Get("loaded_bundle") == nil {
				r.idleSince.Store(id, now)
				continue
			}
			raw, loaded := r.idleSince.LoadOrStore(id, now)
			if !loaded {
				continue
			}
			since := raw.(time.Time)
			if now.Sub(since) < time.Duration(view.Member.Config.IdleUnloadMinutes*float64(time.Minute)) {
				continue
			}
			if oldest == nil || since.Before(oldestTime) || (since.Equal(oldestTime) && id < oldest.Member.ID) {
				copy := view
				oldest = &copy
				oldestTime = since
			}
		}
		if oldest == nil || oldest.Member.ID != member.ID {
			return nil
		}
		// Even a failing worker must not indefinitely block other eligible idle lanes.
		r.idleSince.Store(member.ID, now)
		return oldest
	}()
	if selected == nil {
		return
	}
	// Admission may enqueue while this call runs; the lane reloads on its next tick.
	_, _ = member.Client.JSON("/v1/model/unload", "POST", jsonv.O("protocol", selected.Probe.Protocol), "")
}
