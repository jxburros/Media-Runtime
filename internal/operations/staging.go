package operations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const stagingName = ".media-runtime-downloads"
const stagingOwner = "media-runtime-download-staging-v1\n"

// Validate the publication namespace independently of URL validation: test clients
// may use loopback, but must exercise exactly the same filesystem checks.
func publicationPaths(files []DownloadFile) error {
	seen := map[string]bool{}
	for _, file := range files {
		if !safeRelative(file.Path) || file.Path == "." || file.Path == stagingName || strings.HasPrefix(file.Path, stagingName+"/") || seen[file.Path] {
			return errors.New("unsafe or duplicate publication path")
		}
		seen[file.Path] = true
	}
	for path := range seen {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if seen[parent] {
				return errors.New("manifest files cannot be ancestors of one another")
			}
		}
	}
	return nil
}

// stagingArea reserves a private namespace explicitly marked as Runtime-owned.
// An existing unrelated directory is refused, never adopted or cleaned up.
func stagingArea(root string, m DownloadManifest) (string, *os.File, error) {
	base, err := safeDestination(root, stagingName)
	if err != nil {
		return "", nil, err
	}
	err = os.Mkdir(base, 0700)
	if err == nil {
		owner, err := os.OpenFile(filepath.Join(base, "owner"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", nil, err
		}
		_, writeErr := owner.WriteString(stagingOwner)
		syncErr := owner.Sync()
		closeErr := owner.Close()
		if writeErr != nil {
			return "", nil, writeErr
		}
		if syncErr != nil {
			return "", nil, syncErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", nil, errors.New("download staging must be a private directory")
	}
	ownerPath, err := safeDestination(root, filepath.Join(stagingName, "owner"))
	if err != nil {
		return "", nil, err
	}
	owner, err := os.ReadFile(ownerPath)
	if err != nil || string(owner) != stagingOwner {
		return "", nil, errors.New("existing staging directory is not Runtime-owned")
	}
	encoded, _ := json.Marshal(m)
	digest := sha256.Sum256(encoded)
	name := hex.EncodeToString(digest[:])
	directory, err := safeDestination(base, name)
	if err != nil {
		return "", nil, err
	}
	if err = os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	info, err = os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", nil, errors.New("manifest staging must be a private directory")
	}
	lockPath, err := safeDestination(directory, "lock")
	if err != nil {
		return "", nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return "", nil, errors.New("manifest download is already running")
	}
	return directory, lock, nil
}
func stagedFile(directory string, item DownloadFile) (string, error) {
	encoded, _ := json.Marshal(item)
	digest := sha256.Sum256(encoded)
	path, err := safeDestination(directory, hex.EncodeToString(digest[:])+".part")
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", errors.New("staged download must be a regular file")
	}
	return path, nil
}
