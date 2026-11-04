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

// snapshotRoot locates the revision ancestor of a selected file, including
// nested component files, without resolving away the selected logical path.
func snapshotRoot(path string) string {
	for root := filepath.Dir(path); root != filepath.Dir(root); root = filepath.Dir(root) {
		if snapshotRevision.MatchString(filepath.Base(root)) && filepath.Base(filepath.Dir(root)) == "snapshots" {
			return root
		}
	}
	return ""
}

// ApprovedSnapshotFile is the shared scanner/registration rule for a selected
// file symlink: immutable snapshot identity, unchanged ancestor directories,
// and a regular-file target confined to that repository's blobs directory.
func ApprovedSnapshotFile(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil || parent != filepath.Dir(abs) {
		return false
	}
	root := snapshotRoot(abs)
	return root != "" && approvedSnapshotLink(root, abs)
}
