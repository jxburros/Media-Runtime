package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jxburros/media-runtime/internal/client"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/operations"
)

func localOperations(args []string, o *options) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "service":
		if len(args) != 2 {
			return true, errors.New("service install|uninstall|status --instance NAME --config FILE")
		}
		name, _ := o.get("instance")
		if name == "" {
			name = "default"
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(name) {
			return true, errors.New("invalid service instance")
		}
		configPath, _ := o.get("config")
		if configPath == "" {
			configPath = "runtime.config.json"
		}
		configPath, err := filepath.Abs(configPath)
		if err != nil {
			return true, err
		}
		home, err := os.UserConfigDir()
		if err != nil {
			return true, err
		}
		directory := filepath.Join(home, "systemd", "user")
		unit := "media-runtime-" + name + ".service"
		path := filepath.Join(directory, unit)
		systemctl := func(actions ...string) error {
			command := exec.Command("systemctl", append([]string{"--user"}, actions...)...)
			command.Stdout = os.Stdout
			command.Stderr = os.Stderr
			return command.Run()
		}
		switch args[1] {
		case "install":
			binary, err := os.Executable()
			if err != nil {
				return true, err
			}
			binary, err = filepath.EvalSymlinks(binary)
			if err != nil {
				return true, err
			}
			if err = os.MkdirAll(directory, 0700); err != nil {
				return true, err
			}
			// systemd percent specifiers are escaped; ExecStart is not a shell command.
			quote := func(value string) string { return strconv.Quote(strings.ReplaceAll(value, "%", "%%")) }
			contents := "[Unit]\nDescription=Media Runtime " + name + "\nAfter=network.target\n\n[Service]\nType=simple\nExecStart=" + quote(binary) + " serve --config " + quote(configPath) + "\nRestart=on-failure\nUMask=0077\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n"
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return true, err
			}
			_, err = f.WriteString(contents)
			f.Close()
			if err != nil {
				return true, err
			}
			if err = systemctl("daemon-reload"); err != nil {
				return true, err
			}
			return true, systemctl("enable", unit)
		case "uninstall":
			if _, err = os.Stat(path); err != nil {
				return true, err
			}
			if err = systemctl("disable", "--now", unit); err != nil {
				return true, err
			}
			if err = os.Remove(path); err != nil {
				return true, err
			}
			return true, systemctl("daemon-reload")
		case "status":
			return true, systemctl("status", unit)
		default:
			return true, errors.New("service install|uninstall|status")
		}
	case "downloads", "engine":
		if len(args) != 3 || args[1] != "install" {
			return true, errors.New("downloads install MANIFEST --directory DIR or engine install MANIFEST --directory DIR")
		}
		directory, _ := o.get("directory")
		if directory == "" {
			return true, errors.New("--directory is required")
		}
		manifest, err := operations.ReadManifest(args[2])
		if err != nil {
			return true, err
		}
		if args[0] == "downloads" && manifest.Revision == "" {
			return true, errors.New("model downloads require a pinned revision")
		}
		downloadDir := directory
		if args[0] == "engine" {
			if manifest.Version == "" || len(manifest.Files) != 1 {
				return true, errors.New("engine manifest needs version and exactly one pinned archive")
			}
			downloadDir = filepath.Join(directory, "archives")
		}
		if err = operations.Download(context.Background(), manifest, downloadDir); err != nil {
			return true, err
		}
		if args[0] == "engine" {
			err = operations.InstallEngine(filepath.Join(downloadDir, manifest.Files[0].Path), filepath.Join(directory, manifest.Version))
			if err != nil {
				return true, err
			}
		}
		fmt.Println(string(jsonv.MustMarshal(jsonv.O("installed", true, "directory", directory, "version", manifest.Version, "revision", manifest.Revision))))
		return true, nil
	case "update":
		var version string
		var err error
		if len(args) == 1 {
			version, err = operations.CheckUpdate(context.Background())
		} else if len(args) == 2 {
			var manifest operations.DownloadManifest
			manifest, err = operations.ReadManifest(args[1])
			version = manifest.Version
		} else {
			return true, errors.New("update [MANIFEST] (check only)")
		}
		if err != nil {
			return true, err
		}
		newer, err := operations.NewerVersion(version, c.VERSION)
		if err != nil {
			return true, err
		}
		fmt.Println(string(jsonv.MustMarshal(jsonv.O("current_version", c.VERSION, "candidate_version", version, "update_available", newer, "installed", false))))
		return true, nil
	}
	return false, nil
}
func remoteOperations(cl *client.Client, args []string) (bool, any, error) {
	if len(args) == 0 || args[0] != "folders" {
		return false, nil, nil
	}
	if len(args) == 2 && args[1] == "list" {
		v, err := cl.JSON("/v1/folders", "GET", nil)
		return true, v, err
	}
	if len(args) == 3 && args[1] == "add" {
		path, err := filepath.Abs(args[2])
		if err != nil {
			return true, nil, err
		}
		v, err := cl.JSON("/v1/folders", "POST", jsonv.O("path", path))
		return true, v, err
	}
	if len(args) == 3 && args[1] == "remove" {
		v, err := cl.JSON("/v1/folders/"+args[2], "DELETE", nil)
		return true, v, err
	}
	return true, nil, errors.New("folders add PATH | list | remove ID")
}
