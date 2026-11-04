package runtime

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

func TestCacheIdentitySeparatesSemanticAndSecurityContexts(t *testing.T) {
	r := &Runtime{}
	op := jsonv.O("task", "text-to-image", "inputs", jsonv.O("prompt", "private"), "parameters", jsonv.O("seed", 7.0), "model", jsonv.O("bundle", jsonv.O("bundle_digest", "model-sha")), "backend", jsonv.O("worker_version", "1.0", "resolved_options", jsonv.O("precision", "fp32")), "session_id", "session-1")
	job := jsonv.O("namespace", "owner", "key_id", "key-1", "worker_id", "worker-1")
	original, err := r.cacheIdentity(job, op)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		job, op *jsonv.Object
	}{
		{"namespace", job.Clone().Set("namespace", "other"), op},
		{"credential", job.Clone().Set("key_id", "key-2"), op},
		{"worker", job.Clone().Set("worker_id", "worker-2"), op},
		{"session", job, op.Clone().Set("session_id", "session-2")},
		{"seed", job, op.Clone().Set("parameters", jsonv.O("seed", 8.0))},
		{"prompt", job, op.Clone().Set("inputs", jsonv.O("prompt", "different"))},
		{"model", job, op.Clone().Set("model", jsonv.O("bundle", jsonv.O("bundle_digest", "other")))},
		{"backend", job, op.Clone().Set("backend", jsonv.O("worker_version", "2.0"))},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.cacheIdentity(tt.job, tt.op)
			if err != nil || got == original {
				t.Fatalf("identity collision: %s %v", got, err)
			}
		})
	}
	renamed, err := r.cacheIdentity(job, op.Clone().Set("op_id", "renamed"))
	if err != nil || renamed != original {
		t.Fatal("operation labels should not affect content identity")
	}
}

func TestReceiveBodyRefusesCorruptCacheBytes(t *testing.T) {
	data := []byte("expected-output")
	record := jsonv.O("size", float64(len(data)), "sha256", c.SHA256Hex(data))
	destination := filepath.Join(t.TempDir(), "output.bin")
	check := func(_ io.ReaderAt, _ int64, _ *jsonv.Object) bool { return true }
	if err := receiveBody(bytes.NewReader([]byte("corrupted-data!")), destination, 100, record, check); err == nil {
		t.Fatal("corrupt cache accepted")
	}
	if exists(destination) || exists(destination+".tmp") {
		t.Fatal("partial output retained")
	}
	if err := receiveBody(bytes.NewReader(data), destination, 100, record, check); err != nil {
		t.Fatal(err)
	}
}
