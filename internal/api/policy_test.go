package api

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/security"
	"testing"
)

func TestModelScopeIncludesUnpublishedOperations(t *testing.T) {
	q := &request{key: &security.Key{Policy: security.Policy{Models: []string{"allowed"}}}}
	job := jsonv.O("model", jsonv.O("model_id", "allowed"), "operations", []any{jsonv.O("model", jsonv.O("model_id", "allowed")), jsonv.O("model", jsonv.O("model_id", "secret"))})
	if q.canReadJob(job) {
		t.Fatal("later model escaped policy")
	}
	job.Arr("operations")[1].(*jsonv.Object).Obj("model").Set("model_id", "allowed")
	if !q.canReadJob(job) {
		t.Fatal("allowed graph refused")
	}
	report := q.filterDiagnostics(jsonv.O("models", []any{jsonv.O("model_id", "allowed"), jsonv.O("model_id", "secret")}))
	if len(report.Arr("models")) != 1 {
		t.Fatal("diagnostics leaked restricted model")
	}
}

func TestDiagnosticsScopesLoadedDigestsInBothWorkerViews(t *testing.T) {
	secret := jsonv.O("worker_id", "legacy", "loaded_bundle_digest", "sha256:secret", "applied_fallbacks", []any{"cpu"})
	report := jsonv.O("worker", secret, "workers", []any{
		secret, jsonv.O("worker_id", "permitted", "loaded_bundle_digest", "sha256:allowed"),
		jsonv.O("worker_id", "unregistered", "loaded_bundle_digest", "sha256:unregistered"),
		jsonv.O("worker_id", "idle", "loaded_bundle_digest", nil),
	}, "models", []any{
		jsonv.O("model_id", "allowed", "bundle_digest", "sha256:allowed"),
		jsonv.O("model_id", "secret", "bundle_digest", "sha256:secret"),
		// An allowed alias authorizes this bundle, even if another ID is restricted.
		jsonv.O("model_id", "restricted-alias", "bundle_digest", "sha256:allowed"),
	})
	q := &request{key: &security.Key{Policy: security.Policy{Models: []string{"allowed"}}}}
	filtered := q.filterDiagnostics(report)
	if filtered.Obj("worker").Get("loaded_bundle_digest") != nil || len(filtered.Obj("worker").Arr("applied_fallbacks")) != 0 {
		t.Fatal("legacy worker leaks restricted loaded identity")
	}
	for _, index := range []int{0, 2, 3} {
		if filtered.Arr("workers")[index].(*jsonv.Object).Get("loaded_bundle_digest") != nil {
			t.Fatalf("worker %d leaks restricted/unknown digest", index)
		}
	}
	if filtered.Arr("workers")[1].(*jsonv.Object).Str("loaded_bundle_digest") != "sha256:allowed" {
		t.Fatal("authorized bundle hidden")
	}
	if len(filtered.Arr("models")) != 1 {
		t.Fatal("restricted registry metadata retained")
	}
	if report.Obj("worker").Str("loaded_bundle_digest") != "sha256:secret" {
		t.Fatal("filter mutated shared diagnostic report")
	}
	// Legacy bootstrap tokens and unrestricted managed keys retain the full report.
	for _, unscoped := range []*request{{}, {key: &security.Key{}}} {
		untouched := unscoped.filterDiagnostics(report)
		if untouched != report || untouched.Obj("worker").Str("loaded_bundle_digest") != "sha256:secret" || len(untouched.Arr("models")) != 3 {
			t.Fatal("unrestricted diagnostic behavior changed")
		}
	}
}
