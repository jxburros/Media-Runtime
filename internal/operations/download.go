// Package operations implements explicit operator actions, never automatic job downloads.
package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type DownloadFile struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type DownloadManifest struct {
	SchemaVersion int            `json:"schema_version"`
	Version       string         `json:"version"`
	Revision      string         `json:"revision"`
	AllowedHosts  []string       `json:"allowed_hosts"`
	AllowPatterns []string       `json:"allow_patterns"`
	Files         []DownloadFile `json:"files"`
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func ReadManifest(path string) (DownloadManifest, error) {
	var m DownloadManifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1024*1024))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return m, err
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return m, errors.New("manifest must contain exactly one JSON document")
	}
	if m.SchemaVersion != 1 || len(m.Files) == 0 || len(m.AllowedHosts) == 0 {
		return m, errors.New("manifest needs schema_version 1, files and allowed_hosts")
	}
	if m.Revision != "" && !revisionPattern.MatchString(m.Revision) {
		return m, errors.New("revision must be an immutable 40-character commit")
	}
	if err := publicationPaths(m.Files); err != nil {
		return m, err
	}
	seen := map[string]bool{}
	for _, file := range m.Files {
		if !safeRelative(file.Path) || seen[file.Path] || !digestPattern.MatchString(file.SHA256) || file.Size <= 0 {
			return m, errors.New("manifest files need unique safe relative paths, positive sizes and SHA-256")
		}
		seen[file.Path] = true
		if len(m.AllowPatterns) > 0 {
			matched := false
			for _, pattern := range m.AllowPatterns {
				ok, err := filepath.Match(pattern, file.Path)
				if err != nil {
					return m, err
				}
				matched = matched || ok
			}
			if !matched {
				return m, fmt.Errorf("file %s is outside allow_patterns", file.Path)
			}
		}
		if _, err := approvedURL(file.URL, m.AllowedHosts); err != nil {
			return m, err
		}
	}
	return m, nil
}
func safeRelative(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, "../") && !strings.ContainsAny(path, "\\\x00")
}
func approvedURL(raw string, hosts []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, errors.New("downloads require an HTTPS URL without credentials, fragment or custom port")
	}
	for _, host := range hosts {
		if strings.EqualFold(host, u.Hostname()) {
			return u, nil
		}
	}
	return nil, errors.New("download host is not operator-allow-listed")
}
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

// SafeClient resolves once and pins the selected public address for each connection,
// preventing a redirect or DNS rebinding from reaching loopback/private services.
func SafeClient(hosts []string) *http.Client {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("host has no addresses")
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return nil, errors.New("private or reserved network destination refused")
			}
		}
		dialer := net.Dialer{Timeout: 15 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	return &http.Client{Transport: transport, Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		_, err := approvedURL(req.URL.String(), hosts)
		return err
	}}
}
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func safeDestination(root, relative string) (string, error) {
	if !safeRelative(relative) {
		return "", errors.New("unsafe path")
	}
	path := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err == nil && (info.Mode()&os.ModeSymlink != 0) {
			return "", errors.New("destination symlink refused")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return path, nil
}

// Download verifies every byte before atomic publication. Existing verified files are reused;
// incomplete downloads resume inside an explicitly reserved private staging directory.
// User files outside the manifest, including neighboring .part files, are never touched.
func Download(ctx context.Context, m DownloadManifest, directory string) error {
	return download(ctx, m, directory, SafeClient(m.AllowedHosts))
}
func download(ctx context.Context, m DownloadManifest, directory string, client *http.Client) error {
	if err := publicationPaths(m.Files); err != nil {
		return err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if resolved != root {
		return errors.New("download root must not traverse symlinks")
	}
	var needed int64
	for _, f := range m.Files {
		needed += f.Size
		if needed < 0 {
			return errors.New("manifest size overflow")
		}
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(root, &fs); err != nil {
		return err
	}
	if uint64(needed)+64*1024*1024 > fs.Bavail*uint64(fs.Bsize) {
		return errors.New("insufficient free disk space")
	}
	staging, lock, err := stagingArea(root, m)
	if err != nil {
		return err
	}
	defer lock.Close()
	for _, item := range m.Files {
		path, err := safeDestination(root, item.Path)
		if err != nil {
			return err
		}
		if info, err := os.Stat(path); err == nil {
			sum, e := hashFile(path)
			if e == nil && info.Size() == item.Size && sum == item.SHA256 {
				continue
			}
			return fmt.Errorf("destination already exists with different bytes: %s", item.Path)
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		partial, err := stagedFile(staging, item)
		if err != nil {
			return err
		}
		var offset int64
		if info, err := os.Stat(partial); err == nil {
			offset = info.Size()
		}
		if offset > item.Size {
			return errors.New("partial file exceeds manifest size")
		}
		if offset < item.Size {
			req, err := http.NewRequestWithContext(ctx, "GET", item.URL, nil)
			if err != nil {
				return err
			}
			if offset > 0 {
				req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
			}
			response, err := client.Do(req)
			if err != nil {
				return err
			}
			flags := os.O_CREATE | os.O_WRONLY
			if response.StatusCode == http.StatusPartialContent && offset > 0 {
				if !strings.HasPrefix(response.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
					response.Body.Close()
					return errors.New("invalid resume Content-Range")
				}
				flags |= os.O_APPEND
			} else if response.StatusCode == http.StatusOK {
				offset = 0
				flags |= os.O_TRUNC
			} else {
				response.Body.Close()
				return fmt.Errorf("download returned HTTP %d", response.StatusCode)
			}
			file, err := os.OpenFile(partial, flags, 0600)
			if err != nil {
				response.Body.Close()
				return err
			}
			written, copyErr := io.Copy(file, io.LimitReader(response.Body, item.Size-offset+1))
			response.Body.Close()
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
			if written != item.Size-offset {
				return errors.New("download size differs from manifest")
			}
		}
		sum, err := hashFile(partial)
		if err != nil {
			return err
		}
		if sum != item.SHA256 {
			os.Remove(partial)
			return fmt.Errorf("SHA-256 mismatch for %s", item.Path)
		}
		// Hard-link publication is atomic and never replaces a concurrently created file.
		if err = os.Link(partial, path); err != nil {
			return err
		}
		if err = os.Remove(partial); err != nil {
			return err
		}
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
