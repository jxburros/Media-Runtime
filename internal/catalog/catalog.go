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
	family := data.Str("family")
	if family == "" {
		family = "sdxl-base"
	}
	if family == "esrgan" {
		return buildSingleFile(data, root, files)
	}
	if bareCheckpoint(files) {
		return nil, c.Err(400, "unsupported", "A single-file checkpoint is not a bundle: convert it with media-engine convert, then register the converted directory", "format")
	}
	source, err := withConversion(data.Obj("source"), root, files)
	if err != nil {
		return nil, err
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
		"source", source,
	)
	return jsonv.O(
		"model_id", data.Str("model_id"),
		"artifact_handle", handle,
		"family", "sdxl-base",
		"bundle", bundle.Clone().Set("bundle_digest", c.Hash(bundle)),
	), nil
}

// bareCheckpoint is one safetensors file with no pipeline or network configuration beside it.
func bareCheckpoint(files *jsonv.Object) bool {
	weights := 0
	for _, k := range files.Keys() {
		if strings.Contains(k, "/") || k == "model_index.json" || k == "config.json" {
			return false
		}
		if strings.HasSuffix(k, ".safetensors") {
			weights++
		}
	}
	return weights == 1
}

// singleFileLayout reports the weights file of a single-file-safetensors directory: exactly one
// root .safetensors file plus config.json and nothing else.
func singleFileLayout(files *jsonv.Object) (string, bool) {
	weights := ""
	for _, k := range files.Keys() {
		switch {
		case strings.Contains(k, "/"):
			return "", false
		case k == "config.json":
		case strings.HasSuffix(k, ".safetensors") && weights == "":
			weights = k
		default:
			return "", false
		}
	}
	return weights, weights != "" && files.Has("config.json")
}

// networkConfig reads an upscaler's config.json; ok is false unless it names an RRDB network
// with an integer scale.
func networkConfig(root string) (architecture string, scale float64, ok bool) {
	config, err := readJSON(filepath.Join(root, "config.json"))
	if err != nil {
		return "", 0, false
	}
	scale, isNum := config.Num("scale")
	return config.Str("architecture"), scale, isNum && scale == float64(int(scale)) && config.Str("architecture") == "rrdbnet"
}

// buildSingleFile builds an esrgan bundle: one network weights file plus its configuration.
func buildSingleFile(data *jsonv.Object, root string, files *jsonv.Object) (*jsonv.Object, error) {
	handle := data.Str("artifact_handle")
	weights, ok := singleFileLayout(files)
	if !ok {
		return nil, c.Err(400, "unsupported", "An esrgan bundle is one .safetensors network file plus config.json", "configuration")
	}
	if data.Obj("source").Has("conversion") {
		return nil, c.Err(400, "invalid_request", "Conversion provenance applies to converted SDXL bundles", "source.conversion")
	}
	_, scale, ok := networkConfig(root)
	if !ok {
		return nil, c.Err(400, "unsupported", "config.json must describe an rrdbnet network with an integer scale", "configuration")
	}
	components := []any{
		jsonv.O("role", "network", "artifact_handle", handle, "digest", c.Hash(jsonv.O(weights, files.Get(weights))), "tensor_format", "safetensors"),
		jsonv.O("role", "configuration", "artifact_handle", handle, "digest", c.Hash(jsonv.O("config.json", files.Get("config.json"))), "tensor_format", "json"),
	}
	bundle := jsonv.O(
		"schema_version", 1.0,
		"bundle_id", data.Str("model_id"),
		"family", "esrgan",
		"format", "single-file-safetensors",
		"pipeline", "upscale",
		"components", components,
		"configuration", jsonv.O("artifact_handle", handle, "files", files),
		"source", data.Get("source"),
	)
	return jsonv.O(
		"model_id", data.Str("model_id"),
		"artifact_handle", handle,
		"family", "esrgan",
		"scale", scale,
		"bundle", bundle.Clone().Set("bundle_digest", c.Hash(bundle)),
	), nil
}

// readConversion validates a converted directory's conversion.json.
func readConversion(root string) (*jsonv.Object, error) {
	raw, err := readJSON(filepath.Join(root, "conversion.json"))
	if err != nil {
		return nil, err
	}
	record := jsonv.NewObject()
	for _, k := range []string{"source_sha256", "reference_bundle_digest", "converter", "mapped_keys"} {
		if raw.Has(k) {
			record.Set(k, raw.Get(k))
		}
	}
	return c.Validate(c.Conversion, record)
}

// withConversion records conversion.json provenance in the registration source. A supplied
// source.conversion must equal the directory's file; it is never accepted on its own.
func withConversion(source *jsonv.Object, root string, files *jsonv.Object) (*jsonv.Object, error) {
	supplied := source.Get("conversion")
	if !files.Has("conversion.json") {
		if supplied != nil {
			return nil, c.Err(400, "invalid_request", "This bundle has no conversion.json", "source.conversion")
		}
		return source, nil
	}
	conversion, err := readConversion(root)
	if err != nil {
		return nil, c.Err(400, "invalid_request", "conversion.json is missing a required provenance field", "source.conversion")
	}
	if supplied != nil && c.Hash(supplied) != c.Hash(conversion) {
		return nil, c.Err(400, "invalid_request", "source.conversion differs from the bundle's conversion.json", "source.conversion")
	}
	return source.Clone().Set("conversion", conversion), nil
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
		var pipeline, scheduler, scale, conversion any
		family, format := "sdxl-base", "diffusers-safetensors"
		missing := append([]string(nil), ROLES...)
		entry := func() *jsonv.Object {
			complete := len(problems) == 0
			var registration any
			if complete {
				doc := jsonv.O("model_id", artifactHandle, "artifact_handle", artifactHandle)
				if family != "sdxl-base" {
					doc.Set("family", family)
				}
				if conversion != nil {
					doc.Set("source", jsonv.O("conversion", conversion))
				}
				registration = doc
			}
			return jsonv.O(
				"artifact_handle", artifactHandle,
				"present", present,
				"family", family,
				"format", format,
				"pipeline", pipeline,
				"scale", jsonv.Maybe(scale),
				"conversion", jsonv.Maybe(conversion),
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
		if names, err := readdir(root); err == nil && len(symlinks) == 0 && len(forbiddenFiles) == 0 {
			top := jsonv.NewObject()
			for _, name := range names {
				if info, err := os.Lstat(filepath.Join(root, name)); err == nil {
					if info.IsDir() {
						top.Set(name+"/", "")
					} else {
						top.Set(name, "")
					}
				}
			}
			if _, ok := singleFileLayout(top); ok && !top.Has("model_index.json") {
				// A single-file network: the esrgan upscaler layout.
				family, format, pipeline, missing = "esrgan", "single-file-safetensors", "upscale", nil
				roles = jsonv.O(
					"network", jsonv.O("present", true, "safetensors", 1.0, "files", 1.0),
					"configuration", jsonv.O("present", true, "safetensors", 0.0, "files", 1.0),
				)
				if _, s, ok := networkConfig(root); ok {
					scale = s
				} else {
					problems = append(problems, "config.json must describe an rrdbnet network with an integer scale")
				}
				entries = append(entries, entry())
				continue
			}
			if bareCheckpoint(top) {
				format, missing = "single-file-checkpoint", append([]string(nil), ROLES...)
				problems = append(problems, "Single-file checkpoint: convert it with media-engine convert, then catalogue the converted directory")
				entries = append(entries, entry())
				continue
			}
			if top.Has("conversion.json") {
				if record, err := readConversion(root); err == nil {
					conversion = record
				} else {
					problems = append(problems, "conversion.json is missing a required provenance field")
				}
			}
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
