package operations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type WebhookConfig struct {
	URL          string
	Secret       string
	AllowedHosts []string
}
type Completion struct {
	EventID     string `json:"event_id"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	CompletedAt string `json:"completed_at"`
}
type delivery struct {
	Attempts  int   `json:"attempts"`
	Delivered bool  `json:"delivered"`
	Next      int64 `json:"next"`
}
type Webhooks struct {
	mu         sync.Mutex
	path       string
	configs    []WebhookConfig
	deliveries map[string]delivery
	client     func([]string) *http.Client
}

func OpenWebhooks(directory string, configs []WebhookConfig) (*Webhooks, error) {
	w := &Webhooks{path: filepath.Join(directory, "webhooks.json"), configs: configs, deliveries: map[string]delivery{}}
	for _, cfg := range configs {
		if len(cfg.Secret) < 32 {
			return nil, errors.New("webhook signing secrets need at least 32 characters")
		}
		if _, err := approvedURL(cfg.URL, cfg.AllowedHosts); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &w.deliveries)
	return w, err
}
func (w *Webhooks) save() error {
	data, err := json.Marshal(w.deliveries)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(w.path), ".webhooks-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), w.path)
}

// Deliver uses bounded retries and a stable event ID. Only job ID, terminal state and time
// leave the host; prompts, outputs, namespace names and credentials never enter the payload.
func (w *Webhooks) Deliver(ctx context.Context, event Completion) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, cfg := range w.configs {
		key := shortHash(cfg.URL) + ":" + event.EventID
		record := w.deliveries[key]
		if record.Delivered || record.Attempts >= 5 || record.Next > time.Now().Unix() {
			continue
		}
		record.Attempts++
		record.Next = time.Now().Add(time.Duration(1<<record.Attempts) * time.Second).Unix()
		w.deliveries[key] = record
		// Persist before attempting delivery: a crash cannot reset the retry budget.
		if err := w.save(); err != nil {
			return err
		}
		body, err := json.Marshal(event)
		if err != nil {
			return err
		}
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		signature := hmac.New(sha256.New, []byte(cfg.Secret))
		signature.Write([]byte(timestamp + "."))
		signature.Write(body)
		req, err := http.NewRequestWithContext(ctx, "POST", cfg.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Media-Event-Id", event.EventID)
		req.Header.Set("X-Media-Timestamp", timestamp)
		req.Header.Set("X-Media-Signature", "v1="+hex.EncodeToString(signature.Sum(nil)))
		factory := w.client
		if factory == nil {
			factory = SafeClient
		}
		client := factory(cfg.AllowedHosts)
		client.Timeout = 10 * time.Second
		// A signed body is bound to exactly one configured destination. Never follow POST redirects.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("webhook redirects refused") }
		response, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			record.Delivered = response.StatusCode >= 200 && response.StatusCode < 300
		}
		w.deliveries[key] = record
		if err := w.save(); err != nil {
			return err
		}
	}
	return nil
}
func CompletionEvent(job, state, completed string) Completion {
	return Completion{EventID: fmt.Sprintf("completion_%s", job), JobID: job, State: state, CompletedAt: completed}
}
