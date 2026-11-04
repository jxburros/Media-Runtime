package operations

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadResumesAndVerifiesBytes(t *testing.T) {
	payload := []byte("pinned model content")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	resumed := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=5-" {
			resumed = true
			w.Header().Set("Content-Range", "bytes 5-19/20")
			w.WriteHeader(206)
			w.Write(payload[5:])
			return
		}
		w.Write(payload)
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "model.safetensors.part"), payload[:5], 0600); err != nil {
		t.Fatal(err)
	}
	manifest := DownloadManifest{Files: []DownloadFile{{Path: "model.safetensors", URL: server.URL, SHA256: digest, Size: int64(len(payload))}}}
	if err := download(context.Background(), manifest, root, server.Client()); err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("resume range not sent")
	}
	bytes, err := os.ReadFile(filepath.Join(root, "model.safetensors"))
	if err != nil || string(bytes) != string(payload) {
		t.Fatal("wrong downloaded bytes", err)
	}
	// An attacker-controlled preexisting file is never overwritten.
	manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	if err := download(context.Background(), manifest, root, server.Client()); err == nil {
		t.Fatal("overwrote an existing different file")
	}
}
func TestDownloadRejectsTamperingAndSymlinks(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("bad")) }))
	defer server.Close()
	m := DownloadManifest{Files: []DownloadFile{{Path: "model", URL: server.URL, SHA256: strings.Repeat("0", 64), Size: 3}}}
	root := t.TempDir()
	if err := download(context.Background(), m, root, server.Client()); err == nil {
		t.Fatal("accepted wrong digest")
	}
	if _, err := os.Stat(filepath.Join(root, "model")); err == nil {
		t.Fatal("published wrong bytes")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "model.part")); err != nil {
		t.Fatal(err)
	}
	if err := download(context.Background(), m, root, server.Client()); err == nil {
		t.Fatal("accepted symlink")
	}
}
func TestNetworkDestinationsAreExplicitAndPublic(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "198.18.0.1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Fatal("accepted", ip)
		}
	}
	if !publicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("refused public IP")
	}
	for _, raw := range []string{"http://models.example/a", "https://user:pass@models.example/a", "https://models.example:444/a", "https://elsewhere.example/a"} {
		if _, err := approvedURL(raw, []string{"models.example"}); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, path := range []string{"../a", "/tmp/a", "a/../b", "a\\b"} {
		if safeRelative(path) {
			t.Fatal("accepted unsafe path", path)
		}
	}
}
func TestWatchedFoldersKeepDeletionDistinctAndNeverDeleteSources(t *testing.T) {
	state := t.TempDir()
	root := t.TempDir()
	lora := filepath.Join(root, "loras")
	os.Mkdir(lora, 0700)
	source := filepath.Join(lora, "one.safetensors")
	os.WriteFile(source, []byte("fixture"), 0600)
	m, err := OpenFolders(state)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := m.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := m.Scan()
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if m.List()[0].Entries[0].Kind != "lora" {
		t.Fatal("ComfyUI LoRA tree not recognized")
	}
	os.Remove(source)
	m.Scan()
	if m.List()[0].Entries[0].State != "deleted" {
		t.Fatal("deletion was not recorded")
	}
	os.WriteFile(source, []byte("fixture"), 0600)
	if err = m.Remove(folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); err != nil {
		t.Fatal("remove deleted model", err)
	}
}
func TestEngineArchiveCannotEscapeInstallDirectory(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "engine.tar.gz")
	file, _ := os.Create(archive)
	z := gzip.NewWriter(file)
	tarball := tar.NewWriter(z)
	tarball.WriteHeader(&tar.Header{Name: "../escape", Mode: 0700, Size: 4, Typeflag: tar.TypeReg})
	tarball.Write([]byte("evil"))
	tarball.Close()
	z.Close()
	file.Close()
	if err := InstallEngine(archive, t.TempDir()); err == nil {
		t.Fatal("accepted archive traversal")
	}
}

func TestWebhookSignsPrivatePayloadAndPersistsCompletion(t *testing.T) {
	var calls int
	var received []byte
	var timestamp, signature string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		received, _ = io.ReadAll(r.Body)
		timestamp = r.Header.Get("X-Media-Timestamp")
		signature = r.Header.Get("X-Media-Signature")
		w.WriteHeader(204)
	}))
	defer server.Close()
	secret := strings.Repeat("a", 32)
	cfg := []WebhookConfig{{URL: "https://events.example/media", Secret: secret, AllowedHosts: []string{"events.example"}}}
	directory := t.TempDir()
	hooks, err := OpenWebhooks(directory, cfg)
	if err != nil {
		t.Fatal(err)
	}
	testClient := func([]string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			target, _ := url.Parse(server.URL)
			request.URL.Scheme = target.Scheme
			request.URL.Host = target.Host
			return server.Client().Transport.RoundTrip(request)
		})}
	}
	hooks.client = testClient
	event := CompletionEvent("job_123", "succeeded", "2026-11-04T00:00:00Z")
	if err = hooks.Deliver(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(received)
	if signature != "v1="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature differs")
	}
	var decoded map[string]any
	if err = json.Unmarshal(received, &decoded); err != nil || len(decoded) != 4 {
		t.Fatal("unexpected private payload", string(received), err)
	}
	reopened, err := OpenWebhooks(directory, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reopened.client = testClient
	if err = reopened.Deliver(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("redelivered persisted completion")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateUsesSemanticVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		candidate, current string
		want               bool
	}{{"1.10.0", "1.9.0", true}, {"1.0.0", "1.0.0-rc.2", true}, {"1.0.0-rc.2", "1.0.0", false}, {"0.9.0", "1.0.0", false}, {"1.0.0-rc.10", "1.0.0-rc.2", true}} {
		got, err := NewerVersion(tc.candidate, tc.current)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}
