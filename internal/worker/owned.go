package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

type processIdentity struct {
	BootID     string `json:"boot_id"`
	PID        int    `json:"pid"`
	Start      string `json:"start_ticks"`
	Executable string `json:"executable"`
}

func processInfo(pid int) (processIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processIdentity{}, err
	}
	end := strings.LastIndex(string(data), ")")
	if end < 0 {
		return processIdentity{}, errors.New("invalid process identity")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 {
		return processIdentity{}, errors.New("invalid process identity")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processIdentity{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{BootID: strings.TrimSpace(string(boot)), PID: pid, Start: fields[19], Executable: exe}, nil
}
func (w *Client) pidFile() string { return filepath.Join(w.cfg.Directory, "workers", w.ID+".pid.json") }
func privateWrite(path string, bytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(bytes); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (w *Client) recordProcess(pid int) error {
	identity, err := processInfo(pid)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(identity)
	return privateWrite(w.pidFile(), data)
}
func (w *Client) reapOrphan() error {
	data, err := os.ReadFile(w.pidFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var recorded processIdentity
	if json.Unmarshal(data, &recorded) != nil || recorded.PID <= 1 || recorded.Start == "" || recorded.Executable == "" || recorded.BootID == "" {
		return errors.New("invalid owned worker PID record; inspect it before restarting")
	}
	current, err := processInfo(recorded.PID)
	if errors.Is(err, os.ErrNotExist) {
		return os.Remove(w.pidFile())
	}
	if err != nil {
		return err
	}
	if current != recorded {
		return os.Remove(w.pidFile())
	} // PID was reused: never signal the new owner.
	if err = syscall.Kill(recorded.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		next, err := processInfo(recorded.PID)
		if err != nil || next != recorded {
			return os.Remove(w.pidFile())
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Recheck both executable and start time immediately before escalating.
	if next, err := processInfo(recorded.PID); err == nil && next == recorded {
		if err = syscall.Kill(recorded.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return os.Remove(w.pidFile())
}
func (w *Client) writeCatalog() (string, error) {
	path := filepath.Join(w.cfg.Directory, "workers", w.ID+".catalog.json")
	if err := privateWrite(path, jsonv.MustMarshal(w.cfg.CatalogSnapshot())); err != nil {
		return "", err
	}
	return path, nil
}

// RefreshCatalog publishes newly discovered approved handles for the next Engine lookup/load.
func (w *Client) RefreshCatalog() error {
	if len(w.entry.Command) == 0 || w.entry.Remote {
		return nil
	}
	_, err := w.writeCatalog()
	return err
}
