// Package catalog builds model and adapter registrations from approved catalog directories and
// inspects those directories for the installation workflow. No path leaves this package.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

var forbidden = regexp.MustCompile(`\.(py|pkl|pickle|bin|pt|pth)$`)

var ROLES = []string{"unet", "vae", "text_encoder", "text_encoder_2", "tokenizer", "tokenizer_2", "scheduler"}
var WEIGHT_ROLES = ROLES[:4]

func realpath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func readdir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	return names, nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func join(relative, name string) string {
	if relative == "" {
		return name
	}
	return relative + "/" + name
}

// BuildBundle validates a registration document and hashes the approved SDXL bundle.
func BuildBundle(raw any, catalog *jsonv.Object) (*jsonv.Object, error) {
	data, err := c.Validate(c.Registration, raw)
	if err != nil {
		return nil, err
	}
	handle := data.Str("artifact_handle")
	configured := catalog.Str(handle)
	if configured == "" {
		return nil, c.Err(400, "missing_component", "Artifact handle is not in the operator catalog", "artifact_handle")
	}
	root, err := realpath(configured)
	if err != nil {
		return nil, c.Err(400, "missing_component", "Approved bundle directory is unavailable")
	}
	files := jsonv.NewObject()
	var walk func(relative string) error
	walk = func(relative string) error {
		names, err := readdir(filepath.Join(root, relative))
		if err != nil {
			return err
		}
		for _, name := range names {
			key := join(relative, name)
			path := filepath.Join(root, key)
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				return c.Err(400, "unsupported", "Bundle symlinks are not supported")
			case info.IsDir():
				if err := walk(key); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				if forbidden.MatchString(name) {
					return c.Err(400, "unsupported", "Use a complete safetensors-only Diffusers bundle")
				}
				digest, err := fileDigest(path)
				if err != nil {
					return err
				}
				files.Set(key, digest)
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		var api *c.ApiError
		if errors.As(err, &api) {
			return nil, api
		}
		return nil, c.Err(400, "missing_component", "Cannot read the approved bundle")
	}
	index, err := readJSON(filepath.Join(root, "model_index.json"))
	if err != nil {
		return nil, c.Err(400, "missing_component", "Missing pipeline configuration")
	}
	if index.Get("_class_name") != "StableDiffusionXLPipeline" {
		return nil, c.Err(400, "unsupported", "Only SDXL base Diffusers bundles are supported")
	}
	components := []any{}
	for i, role := range ROLES {
		entries := jsonv.NewObject()
		weights := false
		for _, k := range files.Keys() {
			if strings.HasPrefix(k, role+"/") {
				entries.Set(k, files.Get(k))
				weights = weights || strings.HasSuffix(k, ".safetensors")
			}
		}
		if entries.Len() == 0 || (i < 4 && !weights) {
			return nil, c.Err(400, "missing_component", fmt.Sprintf("Missing %s component", role), role)
		}
		format := "json"
		if i < 4 {
			format = "safetensors"
		}
		components = append(components, jsonv.O("role", role, "artifact_handle", handle, "digest", c.Hash(entries), "tensor_format", format))
	}
	bundle := jsonv.O(
		"schema_version", 1.0,
		"bundle_id", data.Str("model_id"),
		"family", "sdxl-base",
		"pipeline", "text-to-image",
		"components", components,
		"configuration", jsonv.O("artifact_handle", handle, "files", files),
		"source", data.Get("source"),
	)
	return jsonv.O(
		"model_id", data.Str("model_id"),
		"artifact_handle", handle,
		"bundle", bundle.Clone().Set("bundle_digest", c.Hash(bundle)),
	), nil
}

func readJSON(path string) (*jsonv.Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := jsonv.Parse(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*jsonv.Object)
	if !ok {
		// JSON.parse accepts any value; property reads on it yield undefined.
		return jsonv.NewObject(), nil
	}
	return o, nil
}

func className(o *jsonv.Object) string {
	v := o.Get("_class_name")
	if v == nil {
		return "unknown"
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return jsonv.Number(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	if _, ok := v.([]any); ok {
		return strings.Trim(string(jsonv.MustMarshal(v)), "[]")
	}
	return "[object Object]"
}

// Inspect lists each catalog handle's completeness without hashing or registering anything.
func Inspect(catalog *jsonv.Object, handle string) []any {
	entries := []any{}
	for _, artifactHandle := range catalog.Keys() {
		if handle != "" && handle != artifactHandle {
			continue
		}
		configured := catalog.Str(artifactHandle)
		roles := jsonv.NewObject()
		for _, r := range ROLES {
			roles.Set(r, jsonv.O("present", false, "safetensors", 0.0, "files", 0.0))
		}
		var symlinks, forbiddenFiles, problems []string
		present, fileCount, totalBytes := false, 0, int64(0)
		var pipeline, scheduler any
		missing := append([]string(nil), ROLES...)
		entry := func() *jsonv.Object {
			complete := len(problems) == 0
			var registration any
			if complete {
				registration = jsonv.O("model_id", artifactHandle, "artifact_handle", artifactHandle)
			}
			return jsonv.O(
				"artifact_handle", artifactHandle,
				"present", present,
				"pipeline", pipeline,
				"roles", roles,
				"missing_roles", strs(missing),
				"forbidden_files", strs(forbiddenFiles),
				"symlinks", strs(symlinks),
				"scheduler", scheduler,
				"total_bytes", float64(totalBytes),
				"file_count", float64(fileCount),
				"complete", complete,
				"problems", strs(problems),
				"registration", registration,
			)
		}
		root, err := realpath(configured)
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(root)
			if err == nil && !info.IsDir() {
				err = errors.New("not a directory")
			}
		}
		if err != nil {
			problems = append(problems, "Approved directory is missing or not a directory")
			entries = append(entries, entry())
			continue
		}
		present = true
		var walk func(relative string) error
		walk = func(relative string) error {
			names, err := readdir(filepath.Join(root, relative))
			if err != nil {
				return err
			}
			for _, name := range names {
				key := join(relative, name)
				info, err := os.Lstat(filepath.Join(root, key))
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					symlinks = append(symlinks, key)
					continue
				}
				if info.IsDir() {
					if err := walk(key); err != nil {
						return err
					}
				} else if info.Mode().IsRegular() {
					fileCount++
					totalBytes += info.Size()
					if forbidden.MatchString(name) {
						forbiddenFiles = append(forbiddenFiles, key)
					}
					role := strings.Split(key, "/")[0]
					if r := roles.Obj(role); r != nil && strings.Contains(key, "/") {
						n, _ := r.Num("files")
						r.Set("files", n+1)
						r.Set("present", true)
						if strings.HasSuffix(name, ".safetensors") {
							s, _ := r.Num("safetensors")
							r.Set("safetensors", s+1)
						}
					}
				}
			}
			return nil
		}
		if err := walk(""); err != nil {
			problems = append(problems, "Approved directory could not be read")
			entries = append(entries, entry())
			continue
		}
		if index, err := readJSON(filepath.Join(root, "model_index.json")); err == nil {
			pipeline = className(index)
		} else {
			problems = append(problems, "model_index.json is missing or invalid")
		}
		if config, err := readJSON(filepath.Join(root, "scheduler/scheduler_config.json")); err == nil {
			scheduler = className(config)
		} else {
			problems = append(problems, "scheduler/scheduler_config.json is missing or invalid")
		}
		missing = nil
		for i, r := range ROLES {
			role := roles.Obj(r)
			s, _ := role.Num("safetensors")
			if !role.Bool("present") || (i < len(WEIGHT_ROLES) && s == 0) {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, "Missing or weightless components: "+strings.Join(missing, ", "))
		}
		if pipeline != nil && pipeline != "StableDiffusionXLPipeline" {
			problems = append(problems, "Only SDXL base Diffusers bundles are supported")
		}
		if scheduler != nil && scheduler != "EulerDiscreteScheduler" {
			problems = append(problems, "Only the Euler discrete scheduler is supported")
		}
		if len(forbiddenFiles) > 0 {
			problems = append(problems, "Remove Python, pickle and non-safetensors weight files before registering")
		}
		if len(symlinks) > 0 {
			problems = append(problems, "Symlinks are rejected; copy the real files")
		}
		entries = append(entries, entry())
	}
	return entries
}

func strs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// BuildAdapter registers an approved LoRA directory: one safetensors file, hashed.
func BuildAdapter(raw any, catalog *jsonv.Object) (*jsonv.Object, error) {
	data, err := c.Validate(c.AdapterRegistration, raw)
	if err != nil {
		return nil, err
	}
	handle := data.Str("artifact_handle")
	configured := catalog.Str(handle)
	if configured == "" {
		return nil, c.Err(400, "missing_component", "Artifact handle is not in the operator catalog", "artifact_handle")
	}
	root, err := realpath(configured)
	if err != nil {
		return nil, c.Err(400, "missing_component", "Approved adapter directory is unavailable")
	}
	files := jsonv.NewObject()
	err = func() error {
		names, err := readdir(root)
		if err != nil {
			return err
		}
		for _, name := range names {
			info, err := os.Lstat(filepath.Join(root, name))
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return c.Err(400, "unsupported", "Adapter symlinks are not supported")
			}
			if info.IsDir() {
				return c.Err(400, "unsupported", "An adapter directory holds flat files only")
			}
			if forbidden.MatchString(name) {
				return c.Err(400, "unsupported", "Use a safetensors LoRA file")
			}
			digest, err := fileDigest(filepath.Join(root, name))
			if err != nil {
				return err
			}
			files.Set(name, digest)
		}
		return nil
	}()
	if err != nil {
		var api *c.ApiError
		if errors.As(err, &api) {
			return nil, api
		}
		return nil, c.Err(400, "missing_component", "Cannot read the approved adapter")
	}
	weights := 0
	for _, k := range files.Keys() {
		if strings.HasSuffix(k, ".safetensors") {
			weights++
		}
	}
	if weights != 1 {
		return nil, c.Err(400, "unsupported", "An adapter directory contains exactly one .safetensors file")
	}
	record := jsonv.O(
		"schema_version", 1.0,
		"adapter_id", data.Str("adapter_id"),
		"artifact_handle", handle,
		"files", files,
		"source", data.Get("source"),
	)
	return record.Clone().Set("digest", c.Hash(record)), nil
}
