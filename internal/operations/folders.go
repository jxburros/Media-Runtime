package operations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type FolderEntry struct {
	Handle string `json:"artifact_handle"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
}
type Folder struct {
	ID        string        `json:"folder_id"`
	Path      string        `json:"path"`
	Device    uint64        `json:"device"`
	State     string        `json:"state"`
	ScannedAt string        `json:"scanned_at"`
	Entries   []FolderEntry `json:"entries"`
}
type Folders struct {
	mu     sync.Mutex
	path   string
	values []Folder
}

func OpenFolders(directory string) (*Folders, error) {
	m := &Folders{path: filepath.Join(directory, "folders.json"), values: []Folder{}}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &m.values); err != nil {
		return nil, err
	}
	return m, nil
}
func shortHash(s string) string {
	hash := sha256.Sum256([]byte(s))
	return hex.EncodeToString(hash[:8])
}
func device(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Dev)
}
func (m *Folders) save() error {
	data, err := json.MarshalIndent(m.values, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".folders-")
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
	return os.Rename(f.Name(), m.path)
}
func (m *Folders) Add(path string) (Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Folder{}, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return Folder{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return Folder{}, errors.New("folder must be an existing directory")
	}
	for _, f := range m.values {
		if f.Path == absolute {
			return f, nil
		}
	}
	f := Folder{ID: "folder_" + shortHash(absolute), Path: absolute, Device: device(info), State: "online", Entries: []FolderEntry{}}
	scanFolder(&f)
	m.values = append(m.values, f)
	return f, m.save()
}
func (m *Folders) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, f := range m.values {
		if f.ID == id {
			m.values = append(m.values[:i], m.values[i+1:]...)
			return m.save()
		}
	}
	return os.ErrNotExist
}
func (m *Folders) List() []Folder {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := json.Marshal(m.values)
	out := []Folder{}
	_ = json.Unmarshal(data, &out)
	return out
}
func (m *Folders) Scan() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := map[string]string{}
	for i := range m.values {
		scanFolder(&m.values[i])
		for _, entry := range m.values[i].Entries {
			if entry.State == "available" {
				entries[entry.Handle] = entry.Path
			}
		}
	}
	return entries, m.save()
}
func scanFolder(folder *Folder) {
	folder.ScannedAt = time.Now().UTC().Format(time.RFC3339)
	info, err := os.Stat(folder.Path)
	if err != nil || !info.IsDir() {
		folder.State = "missing"
		parent := filepath.Dir(folder.Path)
		for {
			p, e := os.Stat(parent)
			if e == nil {
				if device(p) != folder.Device {
					folder.State = "offline"
				}
				break
			}
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
			parent = next
		}
		for i := range folder.Entries {
			folder.Entries[i].State = folder.State
		}
		return
	}
	if device(info) != folder.Device {
		folder.State = "offline"
		for i := range folder.Entries {
			folder.Entries[i].State = "offline"
		}
		return
	}
	folder.State = "online"
	found := map[string]FolderEntry{}
	count := 0
	err = filepath.WalkDir(folder.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 100000 {
			return errors.New("folder scan exceeds 100000 entries")
		}
		if entry.IsDir() {
			if _, err := os.Stat(filepath.Join(path, "model_index.json")); err == nil {
				kind := "diffusers"
				if strings.Contains(path, string(filepath.Separator)+"snapshots"+string(filepath.Separator)) {
					kind = "huggingface-snapshot"
				}
				handle := "folder_" + shortHash(path)
				found[handle] = FolderEntry{Handle: handle, Path: path, Kind: kind, State: "available"}
				return filepath.SkipDir
			}
			if entry.Name() == ".git" || entry.Name() == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".safetensors") {
			kind := "checkpoint"
			lower := strings.ToLower(path)
			if strings.Contains(lower, "lora") {
				kind = "lora"
			}
			if _, err := os.Stat(path + ".json"); err == nil {
				kind = "esrgan"
			}
			handle := "folder_" + shortHash(path)
			found[handle] = FolderEntry{Handle: handle, Path: path, Kind: kind, State: "available"}
		}
		return nil
	})
	if err != nil {
		folder.State = "unreadable"
		return
	} // A failed traversal must never imply deletion.
	for _, old := range folder.Entries {
		if _, exists := found[old.Handle]; !exists {
			old.State = "deleted"
			found[old.Handle] = old
		}
	}
	folder.Entries = nil
	for _, entry := range found {
		folder.Entries = append(folder.Entries, entry)
	}
	sort.Slice(folder.Entries, func(i, j int) bool { return folder.Entries[i].Path < folder.Entries[j].Path })
}
func (m *Folders) String() string { return fmt.Sprint(len(m.List()), " watched folders") }
