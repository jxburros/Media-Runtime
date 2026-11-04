package runtime

import (
	"bytes"
	"io"
	"regexp"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

const maxPreviewBytes = 1024 * 1024

var previewIDPattern = regexp.MustCompile(`^preview_[a-f0-9]{32}$`)

// rememberPreview retains bounded handle metadata, never image bytes or output records.
func rememberPreview(job, op *jsonv.Object, workerID string, data *jsonv.Object) *jsonv.Object {
	if data == nil || !previewIDPattern.MatchString(data.Str("preview_id")) || data.Str("mime_type") != "image/png" {
		return nil
	}
	size, ok := data.Num("size")
	if !ok || size < 1 || size > maxPreviewBytes {
		return nil
	}
	for _, field := range []string{"width", "height"} {
		n, ok := data.Num(field)
		if !ok || n < 1 || n > 128 {
			return nil
		}
	}
	record := data.Clone().Set("worker_id", workerID).Set("session_id", op.Get("session_id"))
	handles := append(job.Arr("previews"), record)
	if len(handles) > 4 {
		handles = handles[len(handles)-4:]
	}
	job.Set("previews", handles)
	return data.Clone().Set("url", "/v1/jobs/"+job.Str("job_id")+"/previews/"+data.Str("preview_id"))
}

// Preview proxies only an explicitly observed handle for a job in the caller's namespace.
// Previews expire on completion, worker replacement, or eviction and are never durable outputs.
func (r *Runtime) Preview(jobID, previewID, namespace string) ([]byte, string, error) {
	job, err := r.Owned(jobID, namespace)
	if err != nil {
		return nil, "", err
	}
	expired := func() ([]byte, string, error) {
		return nil, "", c.Err(410, "preview_expired", "Preview is no longer available")
	}
	if !previewIDPattern.MatchString(previewID) || c.Terminal(job.Str("state")) {
		return expired()
	}
	var record *jsonv.Object
	for _, raw := range job.Arr("previews") {
		if candidate, ok := raw.(*jsonv.Object); ok && candidate.Str("preview_id") == previewID {
			record = candidate
			break
		}
	}
	if record == nil {
		return expired()
	}
	member := r.Pool.Get(record.Str("worker_id"))
	if member == nil {
		return expired()
	}
	info, err := member.Client.Obj("/v1/info", "GET", nil)
	if err != nil || info.Str("session_id") != record.Str("session_id") {
		return expired()
	}
	response, err := member.Client.Response("/v1/previews/"+previewID, "GET", nil, "")
	if err != nil {
		return expired()
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPreviewBytes+1))
	size, _ := record.Num("size")
	if err != nil || len(data) > maxPreviewBytes || float64(len(data)) != size || response.Header.Get("Content-Type") != "image/png" || c.SHA256Hex(data) != record.Str("sha256") || !c.Signatures["image/png"](bytes.NewReader(data), int64(len(data)), record) {
		return expired()
	}
	return data, "image/png", nil
}
