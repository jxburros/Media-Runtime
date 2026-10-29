package transport

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyCert(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "certs", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TLS files are read with Engine's rules: keys private, certificates not writable by others.
func TestTLSFilesAreReadWithSafeModes(t *testing.T) {
	dir := t.TempDir()
	cert := copyCert(t, dir, "runtime.pem", 0o644)
	key := copyCert(t, dir, "runtime.key", 0o600)
	config, fingerprint, err := Server(cert, key)
	if err != nil || len(config.Certificates) != 1 {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(mustRead(t, cert)))
	parsed, _ := x509.ParseCertificate(block.Bytes)
	if fingerprint != Fingerprint(parsed.Raw) || len(fingerprint) != 64 || strings.ToLower(fingerprint) != fingerprint {
		t.Fatalf("fingerprint %s", fingerprint)
	}
	if err := os.Chmod(key, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Server(cert, key); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a group-readable key: %v", err)
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cert, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Server(cert, key); err == nil || !strings.Contains(err.Error(), "chmod 644") {
		t.Fatalf("a group-writable certificate: %v", err)
	}
	other := copyCert(t, dir, "client.key", 0o600)
	if err := os.Chmod(cert, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Server(cert, other); err == nil || !strings.Contains(err.Error(), "not a matching") {
		t.Fatalf("a key of another certificate: %v", err)
	}
	if _, err := CertPool(key, "ca_file"); err == nil || !strings.Contains(err.Error(), "PEM CA") {
		t.Fatalf("a key as a CA file: %v", err)
	}
	if _, err := CertPool(filepath.Join(dir, "absent.pem"), "ca_file"); err == nil {
		t.Fatal("a missing CA file")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
