// Command media-runtime is the service (serve) and its operator CLI over the public API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jxburros/media-runtime/internal/api"
	"github.com/jxburros/media-runtime/internal/client"
	"github.com/jxburros/media-runtime/internal/config"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/runtime"
	"github.com/jxburros/media-runtime/internal/transport"
	"github.com/jxburros/media-runtime/internal/tui"
)

const help = `Media Runtime %s (Go)

media-runtime serve --config runtime.config.json
media-runtime doctor [--json]
media-runtime models inspect [HANDLE] | models register model.json | models list | models show MODEL [--verify]
media-runtime recipes register recipe.json | recipes list | recipes inspect portrait
media-runtime adapters register adapter.json | adapters list
media-runtime inputs upload image.png | inputs list | inputs delete INPUT
media-runtime run RECIPE --model MODEL [--prompt TEXT] [--image FILE|INPUT] [--strength S] [--adapter ID[:SCALE]]... [--seed N] [--count N]
                   [--tile N] [--tile-overlap N] [--precision P|auto] [--fallback cpu|lower_precision]...
                   [--model-for OP=MODEL]... [--input NAME=VALUE]... [--worker ID] [--wait] [--json]
media-runtime jobs list [--worker ID] | jobs watch JOB | jobs cancel JOB | jobs retry JOB
media-runtime workers list
media-runtime outputs export JOB --directory DIR
media-runtime tui

Client configuration: MEDIA_RUNTIME_URL (https:// beyond loopback), MEDIA_RUNTIME_CA (PEM CA for a
private certificate), MEDIA_RUNTIME_TOKEN, MEDIA_RUNTIME_NAMESPACE.
Exit codes: 0 success, 2 invalid input, 3 unavailable service, 4 failed/cancelled waited job.
`

// exitError carries an exit code without an error message.
type exitError int

func (e exitError) Error() string { return "" }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	strings   map[string]*string
	set       map[string]bool
	adapters  multi
	fallbacks multi
	modelFor  multi
	inputs    multi
	wait      bool
	json      bool
	verify    bool
	help      bool
	version   bool
}

func (o *options) get(name string) (string, bool) { return *o.strings[name], o.set[name] }

// parse reads flags anywhere among the positionals, like Node's util.parseArgs.
func parse(args []string) (*options, []string, error) {
	o := &options{strings: map[string]*string{}, set: map[string]bool{}}
	fs := flag.NewFlagSet("media-runtime", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"config", "model", "prompt", "negative-prompt", "image", "strength", "seed", "width", "height", "steps", "guidance", "precision", "backend", "directory", "idempotency-key", "count", "tile", "tile-overlap", "worker"} {
		o.strings[name] = fs.String(name, "", "")
	}
	fs.Var(&o.adapters, "adapter", "")
	fs.Var(&o.fallbacks, "fallback", "")
	fs.Var(&o.modelFor, "model-for", "")
	fs.Var(&o.inputs, "input", "")
	fs.BoolVar(&o.wait, "wait", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.verify, "verify", false, "")
	fs.BoolVar(&o.help, "help", false, "")
	fs.BoolVar(&o.version, "version", false, "")
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, fmt.Errorf("%s. Use --help.", strings.TrimSuffix(err.Error(), "\n"))
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			positionals = append(positionals, args[1:]...)
			break
		}
		positionals = append(positionals, args[0])
		args = args[1:]
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return o, positionals, nil
}

func main() {
	syscall.Umask(0o077)
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	var code exitError
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	message := err.Error()
	status := 2
	var apiErr *client.Error
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 {
			status = 3
		}
	} else if errors.Is(err, client.ErrUnavailable) {
		status = 3
	}
	fmt.Fprintln(os.Stderr, string(jsonv.MustMarshal(jsonv.O("error", message))))
	os.Exit(status)
}

func run(argv []string) error {
	values, args, err := parse(argv)
	if err != nil {
		return err
	}
	if values.version {
		fmt.Println(c.VERSION)
		return nil
	}
	if values.help || len(args) == 0 {
		fmt.Printf(help+"\n", c.VERSION)
		return nil
	}
	if args[0] == "serve" {
		configPath, _ := values.get("config")
		return serve(configPath)
	}
	token := os.Getenv("MEDIA_RUNTIME_TOKEN")
	if token == "" {
		return errors.New("Set MEDIA_RUNTIME_TOKEN")
	}
	base := os.Getenv("MEDIA_RUNTIME_URL")
	if base == "" {
		base = "http://127.0.0.1:7810"
	}
	cl := client.New(base, token, os.Getenv("MEDIA_RUNTIME_NAMESPACE"))
	if err := cl.Secure(os.Getenv("MEDIA_RUNTIME_CA")); err != nil {
		return err
	}
	print := func(v any) {
		if values.json {
			fmt.Println(string(jsonv.MustMarshal(v)))
		} else {
			fmt.Println(string(jsonv.Indent(v)))
		}
	}
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	watch := func(jobID string) error {
		prior := ""
		for {
			job, err := cl.Status(jobID)
			if err != nil {
				return err
			}
			progress := job.Obj("progress")
			stamp := string(jsonv.MustMarshal([]any{job.Get("state"), job.Get("stage"), job.Get("cancel_requested"), progress.Get("completed_steps"), progress.Get("completed"), job.Get("applied_fallbacks"), job.Get("current_op_id"), job.Get("worker_id")}))
			if stamp != prior {
				print(job)
				prior = stamp
			}
			if c.Terminal(job.Str("state")) {
				if job.Str("state") != "succeeded" {
					return exitError(4)
				}
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	switch {
	case args[0] == "doctor":
		report, err := cl.Health()
		if err != nil {
			return err
		}
		if values.json {
			print(report)
			return nil
		}
		lines, healthy := ExplainDoctor(report)
		fmt.Println(strings.Join(lines, "\n"))
		if !healthy {
			return exitError(3)
		}
		return nil
	case args[0] == "tui":
		return tui.Run(cl, os.Stdin, os.Stdout, time.Second)
	case args[0] == "models" && at(1) == "inspect":
		raw, err := cl.Catalog(at(2))
		if err != nil {
			return err
		}
		entries, _ := raw.([]any)
		if values.json {
			print(raw)
		} else {
			fmt.Println(strings.Join(ExplainCatalog(entries), "\n"))
		}
		for _, e := range entries {
			if o, _ := e.(*jsonv.Object); !o.Bool("complete") {
				return exitError(2)
			}
		}
		return nil
	case args[0] == "models" && at(1) == "show" && at(2) != "":
		detail, err := cl.Model(at(2), values.verify)
		if err != nil {
			return err
		}
		print(detail)
		return nil
	}
	if args[0] == "models" || args[0] == "recipes" || args[0] == "adapters" {
		if at(1) == "register" && at(2) != "" {
			data, err := os.ReadFile(at(2))
			if err != nil {
				return err
			}
			body, err := jsonv.Parse(data)
			if err != nil {
				return fmt.Errorf("%s is not valid JSON", at(2))
			}
			result, err := cl.JSON("/v1/"+args[0], "POST", body)
			if err != nil {
				return err
			}
			print(result)
			return nil
		}
		list, err := cl.JSON("/v1/"+args[0], "GET", nil)
		if err != nil {
			return err
		}
		if at(1) == "list" {
			print(list)
			return nil
		}
		if args[0] == "recipes" && at(1) == "inspect" {
			// The typed declarations, operations and bindings, for schema 1 and 2 alike.
			recipe, err := cl.Recipe(at(2))
			var apiErr *client.Error
			if errors.As(err, &apiErr) && apiErr.Status == 404 {
				return errors.New("Recipe not found")
			}
			if err != nil {
				return err
			}
			print(recipe)
			return nil
		}
	}
	if args[0] == "inputs" {
		switch {
		case at(1) == "upload" && at(2) != "":
			data, err := os.ReadFile(at(2))
			if err != nil {
				return err
			}
			record, err := cl.UploadInput(data)
			if err != nil {
				return err
			}
			print(record)
			return nil
		case at(1) == "list":
			list, err := cl.Inputs()
			if err != nil {
				return err
			}
			print(list)
			return nil
		case at(1) == "delete" && at(2) != "":
			result, err := cl.DeleteInput(at(2))
			if err != nil {
				return err
			}
			print(result)
			return nil
		}
	}
	if args[0] == "run" && at(1) != "" {
		return runJob(cl, values, at(1), print, watch)
	}
	if args[0] == "jobs" {
		switch {
		case at(1) == "list":
			worker, _ := values.get("worker")
			list, err := cl.WorkerJobs(0, 0, false, worker)
			if err != nil {
				return err
			}
			print(list)
			return nil
		case at(1) == "watch" && at(2) != "":
			return watch(at(2))
		case at(1) == "cancel" && at(2) != "":
			job, err := cl.Cancel(at(2))
			if err != nil {
				return err
			}
			print(job)
			return nil
		case at(1) == "retry" && at(2) != "":
			old, err := cl.Status(at(2))
			if err != nil {
				return err
			}
			body := old.Obj("request").Clone().Set("retry_of", old.Get("job_id"))
			accepted, err := cl.Submit(body, "")
			if err != nil {
				return err
			}
			print(accepted)
			return nil
		}
	}
	if args[0] == "workers" && at(1) == "list" {
		list, err := cl.Workers()
		if err != nil {
			return err
		}
		print(list)
		return nil
	}
	if directory, ok := values.get("directory"); args[0] == "outputs" && at(1) == "export" && at(2) != "" && ok && directory != "" {
		return export(cl, at(2), directory, print)
	}
	return errors.New("Unknown or incomplete command. Use --help.")
}

var inputID = regexp.MustCompile(`^input_[a-f0-9]{32}$`)

// number is JavaScript's Number(text): NaN (sent as null) for anything unparseable.
func number(text string) float64 {
	t := strings.TrimSpace(text)
	if t == "" {
		return 0
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return f
}

func runJob(cl *client.Client, values *options, recipe string, print func(any), watch func(string) error) error {
	parameters := jsonv.NewObject()
	for _, field := range []string{"seed", "width", "height", "steps", "guidance", "strength", "count", "tile", "tile-overlap"} {
		if v, ok := values.get(field); ok {
			parameters.Set(strings.ReplaceAll(field, "-", "_"), number(v))
		}
	}
	// A local file is uploaded first; the service never receives a client path.
	upload := func(image string) (string, error) {
		if image == "" || inputID.MatchString(image) {
			return image, nil
		}
		data, err := os.ReadFile(image)
		if err != nil {
			return "", err
		}
		record, err := cl.UploadInput(data)
		if err != nil {
			return "", err
		}
		if !values.json {
			fmt.Fprintln(os.Stderr, string(jsonv.MustMarshal(jsonv.O("uploaded", record))))
		}
		return record.Str("input_id"), nil
	}
	image, _ := values.get("image")
	image, err := upload(image)
	if err != nil {
		return err
	}
	inputs := jsonv.NewObject()
	if prompt, ok := values.get("prompt"); ok {
		inputs.Set("prompt", prompt)
	}
	if negative, _ := values.get("negative-prompt"); negative != "" {
		inputs.Set("negative_prompt", negative)
	}
	if image != "" {
		inputs.Set("image", image)
	}
	if len(values.inputs) > 0 {
		// Named inputs of a schema-2 recipe are typed by its declarations.
		detail, err := cl.Recipe(recipe)
		if err != nil {
			return err
		}
		for _, entry := range values.inputs {
			name, value, ok := strings.Cut(entry, "=")
			if !ok || name == "" {
				return errors.New("Use --input NAME=VALUE")
			}
			switch detail.Obj("inputs").Obj(name).Str("type") {
			case "number":
				inputs.Set(name, number(value))
			case "image":
				id, err := upload(value)
				if err != nil {
					return err
				}
				inputs.Set(name, id)
			default:
				inputs.Set(name, value)
			}
		}
	}
	body := jsonv.O("recipe_id", recipe)
	if model, ok := values.get("model"); ok {
		body.Set("model_id", model)
	}
	body.Set("inputs", inputs).Set("parameters", parameters)
	if len(values.adapters) > 0 {
		list := []any{}
		for _, entry := range values.adapters {
			parts := strings.Split(entry, ":")
			use := jsonv.O("adapter_id", parts[0])
			if len(parts) > 1 {
				use.Set("scale", number(parts[1]))
			}
			list = append(list, use)
		}
		body.Set("adapters", list)
	}
	if len(values.modelFor) > 0 {
		models := jsonv.NewObject()
		for _, entry := range values.modelFor {
			op, model, ok := strings.Cut(entry, "=")
			if !ok || op == "" || model == "" {
				return errors.New("Use --model-for OP=MODEL")
			}
			models.Set(op, model)
		}
		body.Set("models", models)
	}
	backend, _ := values.get("backend")
	if backend != "" {
		body.Set("backend_id", backend)
	}
	if precision, _ := values.get("precision"); precision != "" {
		key := backend
		if key == "" {
			key = "diffusers-sdxl"
		}
		body.Set("backend_options", jsonv.O(key, jsonv.O("precision", precision)))
	}
	if len(values.fallbacks) > 0 {
		list := []any{}
		for _, f := range values.fallbacks {
			list = append(list, f)
		}
		body.Set("authorized_fallbacks", list)
	}
	if worker, _ := values.get("worker"); worker != "" {
		body.Set("worker_id", worker)
	}
	key, _ := values.get("idempotency-key")
	accepted, err := cl.Submit(body, key)
	if err != nil {
		return err
	}
	print(accepted)
	if values.wait {
		return watch(accepted.Str("job_id"))
	}
	return nil
}

func export(cl *client.Client, jobID, target string, print func(any)) error {
	job, err := cl.Status(jobID)
	if err != nil {
		return err
	}
	directory, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if job.Str("state") != "succeeded" {
		return errors.New("Job has no committed outputs")
	}
	write := func(name string, data []byte) error {
		f, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(data)
		return errors.Join(werr, f.Close())
	}
	for _, raw := range job.Arr("outputs") {
		output, _ := raw.(*jsonv.Object)
		res, err := cl.Output(job.Str("job_id"), output.Str("output_id"))
		if err != nil {
			return err
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return err
		}
		ext := c.Extensions[output.Str("mime_type")]
		if ext == "" {
			ext = "bin"
		}
		if err := write(fmt.Sprintf("%s-%s.%s", job.Str("job_id"), output.Str("output_id"), ext), data); err != nil {
			return err
		}
	}
	manifest, err := cl.Manifest(job.Str("job_id"))
	if err != nil {
		return err
	}
	if err := write(job.Str("job_id")+"-manifest.json", jsonv.Indent(manifest)); err != nil {
		return err
	}
	print(jsonv.O("directory", directory))
	return nil
}

// serve owns the state directory through an exclusive lock file until SIGINT or SIGTERM.
func serve(configPath string) error {
	cfg, err := config.Read(configPath)
	if err != nil {
		return err
	}
	// Refuse unusable API TLS material before any worker is started.
	if cfg.TLS != nil {
		if _, _, err := transport.Server(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.Directory, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(cfg.Directory, "service.lock")
	if err := lock(lockPath); err != nil {
		return err
	}
	rt, err := runtime.New(cfg)
	if err != nil {
		os.Remove(lockPath)
		return err
	}
	server := api.New(rt)
	closed := false
	shutdown := func() {
		if closed {
			return
		}
		closed = true
		server.Close()
		rt.Stop()
		os.Remove(lockPath)
	}
	if err := rt.Start(); err != nil {
		shutdown()
		return err
	}
	if err := server.Serve(cfg.Bind, cfg.Port, cfg.TLS); err != nil {
		shutdown()
		return err
	}
	var fingerprint any
	if f := server.Fingerprint(); f != "" {
		fingerprint = f
	}
	fmt.Println(string(jsonv.MustMarshal(jsonv.O("service", "media-runtime", "version", c.VERSION, "port", float64(server.Port()),
		"scheme", server.Scheme(), "bind", cfg.Bind, "certificate_sha256", fingerprint))))
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()
	for {
		select {
		case <-cleanup.C:
			_ = rt.Cleanup()
		case <-signals:
			shutdown()
			return nil
		}
	}
}

func lock(path string) error {
	create := func() error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
		return errors.Join(werr, f.Close())
	}
	err := create()
	if err == nil || !errors.Is(err, os.ErrExist) {
		return err
	}
	data, _ := os.ReadFile(path)
	live := true
	if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			live = false
		}
	} else {
		// Number("garbage") is NaN and process.kill(NaN) fails without ESRCH: treat as live.
		live = true
	}
	if live {
		return errors.New("Another runtime owns this state directory")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return create()
}
