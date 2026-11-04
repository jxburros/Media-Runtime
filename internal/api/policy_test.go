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
