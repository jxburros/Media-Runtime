package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var snapshotRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)

// approvedSnapshotLink admits only Hugging Face's content-addressed snapshot layout.
// Generic bundle symlinks and directory symlinks remain forbidden.
func approvedSnapshotLink(root, path string) bool {
	if !snapshotRevision.MatchString(filepath.Base(root)) || filepath.Base(filepath.Dir(root)) != "snapshots" {
		return false
	}
	repository := filepath.Dir(filepath.Dir(root))
	if !strings.HasPrefix(filepath.Base(repository), "models--") {
		return false
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	blobs := filepath.Join(repository, "blobs")
	rel, err := filepath.Rel(blobs, target)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
