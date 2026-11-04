package runtime

import (
	"time"

	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/pool"
)

// maybeIdleUnload releases a worker only when no active or eligible queued job needs it.
// Each worker lane calls it serially; last-use timestamps provide LRU ordering naturally
// as idle workers reach their own configured deadline. No model is moved between workers.
func (r *Runtime) maybeIdleUnload(member *pool.Member) {
	if member.Config.IdleUnloadMinutes <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.runningOn(member.ID) != nil {
		r.idleSince.Store(member.ID, now)
		return
	}
	for _, job := range r.Store.All("queued") {
		pinned := job.Obj("request").Str("worker_id")
		if pinned == "" || pinned == member.ID {
			r.idleSince.Store(member.ID, now)
			return
		}
	}
	var newest *jsonv.Object
	for _, job := range r.Store.All("") {
		if job.Str("worker_id") == member.ID && (newest == nil || job.Str("updated_at") > newest.Str("updated_at")) {
			newest = job
		}
	}
	if newest != nil && newest.Obj("request").Bool("keep_loaded") {
		r.idleSince.Store(member.ID, now)
		return
	}
	raw, loaded := r.idleSince.LoadOrStore(member.ID, now)
	if !loaded {
		return
	}
	if now.Sub(raw.(time.Time)) < time.Duration(member.Config.IdleUnloadMinutes*float64(time.Minute)) {
		return
	}
	for _, view := range r.Pool.Views() {
		if view.Member.ID != member.ID || !view.Ready() {
			continue
		}
		if view.Probe.Info.Str("state") != "ready" || view.Probe.Info.Get("loaded_bundle") == nil {
			return
		}
		if _, err := member.Client.JSON("/v1/model/unload", "POST", jsonv.O("protocol", view.Probe.Protocol), ""); err == nil {
			r.idleSince.Store(member.ID, now)
		}
	}
}
