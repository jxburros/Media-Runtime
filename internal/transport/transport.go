// Package transport reads TLS material for the two network paths: the runtime's client side of
// mutual TLS to a remote worker, and the API's own HTTPS listener. Everything is standard
// library crypto/tls; the files are read once, at start, with the same mode rules Media Engine
// applies to its own launch files.
package transport

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ReadFile reads one TLS file. A private key must not be readable by group or others; a
// certificate or CA file must not be writable by them.
func ReadFile(path, label string, private bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s could not be read", label)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must name a regular file", label)
	}
	mode := info.Mode().Perm()
	if private && mode&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by group or others (mode %04o); run chmod 600 on it", label, mode)
	}
	if !private && mode&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by group or others (mode %04o); run chmod 644 on it", label, mode)
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("%s could not be read", label)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("%s is too large", label)
	}
	return data, nil
}

// KeyPair loads a PEM certificate chain (leaf first) and its unencrypted PEM private key.
func KeyPair(certFile, keyFile, label string) (tls.Certificate, error) {
	cert, err := ReadFile(certFile, label+".cert_file", false)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ReadFile(keyFile, label+".key_file", true)
	if err != nil {
		return tls.Certificate{}, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s.cert_file and %s.key_file are not a matching PEM certificate and unencrypted key", label, label)
	}
	return pair, nil
}

// CertPool loads the PEM CA certificates a peer's certificate must chain to.
func CertPool(caFile, label string) (*x509.CertPool, error) {
	data, err := ReadFile(caFile, label, false)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s must contain PEM CA certificates", label)
	}
	return pool, nil
}

// Fingerprint is the SHA-256 of a certificate's DER in lowercase hex, the form Media Engine
// reports as certificate_sha256.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// MismatchError is a server certificate whose fingerprint is not the pinned or recorded one.
type MismatchError struct {
	Expected  string
	Presented string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("worker certificate sha256 %s is not the expected %s", e.Presented, e.Expected)
}

// Server is the API's TLS configuration: TLS 1.2 or later with the operator's certificate.
// Browsers and gateways connect with their own trust roots; no client certificate is asked for,
// because the bearer credential authorizes every request.
func Server(certFile, keyFile string) (*tls.Config, string, error) {
	pair, err := KeyPair(certFile, keyFile, "tls")
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, Fingerprint(pair.Certificate[0]), nil
}
