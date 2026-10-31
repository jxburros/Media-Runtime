// Package tui is the terminal dashboard. It consumes the public API only and adds no execution
// behavior: every action it offers (cancel, export hints) is an existing API operation.
package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/media-runtime/internal/client"
	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

type State struct {
	Runtime   *jsonv.Object
	Jobs      []*jsonv.Object
	Selected  int
	Message   string
	Error     string
	UpdatedAt string
}

// width is a string's length in UTF-16 code units, the unit the dashboard was laid out in.
func width(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func pad(text string, w int) string {
	if width(text) > w {
		runes := []rune(text)
		keep := w - 1
		if keep < 0 {
			keep = 0
		}
		if keep > len(runes) {
			keep = len(runes)
		}
		return string(runes[:keep]) + "…"
	}
	return text + strings.Repeat(" ", w-width(text))
}

// js formats a member the way a JavaScript template literal interpolates it.
func js(o *jsonv.Object, key string) string {
	if !o.Has(key) {
		return "undefined"
	}
	return jsString(o.Get(key))
}

func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case float64:
		return jsonv.Number(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

func num(o *jsonv.Object, key, unit string) string {
	if o.Get(key) == nil {
		return "unknown"
	}
	return jsString(o.Get(key)) + unit
}

// DescribeStage is the stage column: sampling progress, the stage, or the state. A linear
// job's running stage is prefixed with its current operation (op_id/stage).
func DescribeStage(job *jsonv.Object) string {
	if op := job.Str("current_op_id"); op != "" && job.Str("state") == "running" {
		return op + "/" + describeStage(job)
	}
	return describeStage(job)
}

func describeStage(job *jsonv.Object) string {
	switch job.Str("state") {
	case "running":
		progress := job.Obj("progress")
		if progress.Str("stage") == "sampling" && truthy(progress.Get("total_steps")) {
			return fmt.Sprintf("sampling %s/%s", js(progress, "completed_steps"), js(progress, "total_steps"))
		}
		if truthy(progress.Get("total_steps")) && progress.Get("unit") != nil {
			// Stage progress counted in the worker's unit: decoding frames, upscaling tiles.
			return fmt.Sprintf("%s %s/%s %s", js(progress, "stage"), js(progress, "completed_steps"), js(progress, "total_steps"), js(progress, "unit"))
		}
		if truthy(progress.Get("total")) && progress.Get("unit") != nil {
			return fmt.Sprintf("%s %s/%s %s", js(progress, "stage"), js(progress, "completed"), js(progress, "total"), js(progress, "unit"))
		}
		if s := job.Str("stage"); s != "" {
			return s
		}
		return "dispatching"
	case "queued":
		if truthy(job.Get("cancel_requested")) {
			return "cancelling"
		}
		return "queued"
	}
	return js(job, "state")
}

// MediaFacts is the media column: a video job's frame count and duration, from its committed
// output record once it succeeded and from its resolved request before. Image jobs have none.
func MediaFacts(job *jsonv.Object) string {
	media := job.Obj("media")
	if media == nil {
		p := job.Obj("request").Obj("parameters")
		frames, fok := p.Num("frames")
		fps, pok := p.Num("fps")
		if !fok || !pok || fps == 0 || job.Str("task") != "image-to-video" {
			return ""
		}
		media = jsonv.O("type", "video", "frames", frames, "duration_seconds", frames/fps)
	}
	duration, ok := media.Num("duration_seconds")
	if !ok {
		return ""
	}
	if media.Str("type") == "video" {
		return fmt.Sprintf("%sf %.2fs", js(media, "frames"), duration)
	}
	return fmt.Sprintf("%.2fs", duration)
}

// FallbackNames lists applied fallbacks, which the worker records as {fallback, reason, …}.
func FallbackNames(applied []any) string {
	names := []string{}
	for _, raw := range applied {
		switch f := raw.(type) {
		case string:
			names = append(names, f)
		case *jsonv.Object:
			names = append(names, f.Str("fallback"))
		}
	}
	return strings.Join(names, ", ")
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	}
	return true
}

// OutputLocation is the API path of a succeeded job's first output, "expired" or "".
func OutputLocation(job *jsonv.Object) string {
	outputs := job.Arr("outputs")
	if job.Str("state") != "succeeded" || len(outputs) == 0 {
		return ""
	}
	output, _ := outputs[0].(*jsonv.Object)
	if c.Expired(output.Get("expires_at")) {
		return "expired"
	}
	return fmt.Sprintf("/v1/jobs/%s/outputs/%s", js(job, "job_id"), js(output, "output_id"))
}

func slice(s string, start, end int) string {
	runes := []rune(s)
	if start > len(runes) {
		return ""
	}
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

// workerLine is one worker's header line: its backend, protocol and device, or its error.
func workerLine(label string, worker *jsonv.Object) string {
	if worker == nil {
		worker = jsonv.NewObject()
	}
	if truthy(worker.Get("error")) {
		message := ""
		if worker.Get("message") != nil {
			message = jsString(worker.Get("message"))
		}
		return fmt.Sprintf("%s: %s — %s", label, js(worker, "error"), message)
	}
	ready := "not ready"
	if truthy(worker.Get("device_ready")) {
		ready = "ready"
	}
	return fmt.Sprintf("%s: %s (%s) %s  %s  device %s  %s  state %s", label,
		js(worker, "backend_id"), js(worker, "ownership"), js(worker, "worker_version"), js(worker, "protocol"),
		js(worker, "device"), ready, js(worker, "state"))
}

// Render is the pure renderer: state in, screen lines out.
func Render(state State, columns, rows int) []string {
	lines := []string{}
	rt := state.Runtime
	worker := rt.Obj("worker")
	if worker == nil {
		worker = jsonv.NewObject()
	}
	queue := rt.Obj("queue")
	if queue == nil {
		queue = jsonv.NewObject()
	}
	version := "?"
	if rt.Get("runtime_version") != nil {
		version = jsString(rt.Get("runtime_version"))
	}
	lines = append(lines, pad(fmt.Sprintf("Media Runtime %s  %s  [q] quit  [j/k] select  [c] cancel  [r] refresh", version, state.UpdatedAt), columns))
	// A pool of one keeps the single worker line; a pool shows one line per worker and a worker
	// column in the job table.
	workers := rt.Arr("workers")
	pool := len(workers) > 1
	// A media column appears only when a listed job has video or audio facts, so image-only
	// dashboards keep their layout.
	media := false
	for _, job := range state.Jobs {
		media = media || MediaFacts(job) != ""
	}
	if pool {
		for _, raw := range workers {
			w, _ := raw.(*jsonv.Object)
			lines = append(lines, pad(workerLine("worker "+js(w, "worker_id"), w), columns))
		}
	} else {
		lines = append(lines, pad(workerLine("worker", worker), columns))
	}
	models := []string{}
	for _, raw := range rt.Arr("models") {
		m, _ := raw.(*jsonv.Object)
		text := js(m, "model_id")
		if truthy(m.Get("loaded")) {
			text += "*"
		}
		if !truthy(m.Get("available")) {
			text += "(missing)"
		}
		models = append(models, text)
	}
	modelText := strings.Join(models, ", ")
	if modelText == "" {
		modelText = "none"
	}
	estimate := worker.Obj("memory_estimate")
	source := "unknown"
	if estimate.Get("source") != nil {
		source = jsString(estimate.Get("source"))
	}
	lines = append(lines, pad(fmt.Sprintf("queue: %s running, %s queued of %s  models: %s  memory: %s (%s)",
		num(queue, "running", ""), num(queue, "queued", ""), num(queue, "capacity", ""), modelText,
		num(estimate, "value_mb", " MiB"), source), columns))
	lines = append(lines, strings.Repeat("─", columns))
	const wID, wState, wTask, wWorker, wMedia, wModel, wSeed, wCreated = 14, 18, 14, 10, 12, 12, 11, 15
	header := []string{pad("job", wID), pad("stage", wState), pad("task", wTask)}
	if pool {
		header = append(header, pad("worker", wWorker))
	}
	if media {
		header = append(header, pad("media", wMedia))
	}
	header = append(header, pad("model", wModel), pad("seed", wSeed), pad("created", wCreated), "error / output")
	lines = append(lines, pad(strings.Join(header, " "), columns))
	available := rows - len(lines) - 4
	if available < 1 {
		available = 1
	}
	start := state.Selected - available + 1
	if alt := len(state.Jobs) - available; alt < start {
		start = alt
	}
	if start < 0 {
		start = 0
	}
	end := start + available
	if end > len(state.Jobs) {
		end = len(state.Jobs)
	}
	for index, job := range state.Jobs[start:end] {
		marker := " "
		if start+index == state.Selected {
			marker = ">"
		}
		location := OutputLocation(job)
		tail := ""
		if e := job.Obj("error"); truthy(job.Get("error")) {
			tail = js(e, "code")
			if truthy(e.Get("forced_termination")) {
				tail += " (forced)"
			}
		} else if location == "expired" {
			tail = "output expired"
		} else if location != "" {
			tail = "output ready"
			if n := len(job.Arr("outputs")); n > 1 {
				tail = fmt.Sprintf("%d outputs ready", n)
			}
		}
		if names := FallbackNames(job.Arr("applied_fallbacks")); names != "" {
			tail += " (fallback " + names + ")"
		}
		task := "text-to-image"
		if job.Get("task") != nil {
			task = jsString(job.Get("task"))
		}
		model := ""
		if job.Obj("model").Get("model_id") != nil {
			model = jsString(job.Obj("model").Get("model_id"))
		}
		seed := ""
		if s := job.Obj("request").Obj("parameters").Get("seed"); s != nil {
			seed = jsString(s)
		}
		created := ""
		if s, ok := job.Get("created_at").(string); ok {
			created = strings.Replace(slice(s, 5, 19), "T", " ", 1)
		}
		row := []string{
			pad(slice(job.Str("job_id"), 4, 4+wID-1), wID-1),
			pad(DescribeStage(job), wState),
			pad(task, wTask),
		}
		if pool {
			row = append(row, pad(job.Str("worker_id"), wWorker))
		}
		if media {
			row = append(row, pad(MediaFacts(job), wMedia))
		}
		row = append(row, pad(model, wModel), pad(seed, wSeed), pad(created, wCreated), tail)
		lines = append(lines, pad(marker+strings.Join(row, " "), columns))
	}
	if len(state.Jobs) == 0 {
		lines = append(lines, pad("  no jobs in this namespace", columns))
	}
	for len(lines) < rows-3 {
		lines = append(lines, strings.Repeat(" ", columns))
	}
	lines = append(lines, strings.Repeat("─", columns))
	detail := "select a job to see its details"
	if state.Selected >= 0 && state.Selected < len(state.Jobs) {
		selected := state.Jobs[state.Selected]
		p := selected.Obj("request").Obj("parameters")
		if p == nil {
			p = jsonv.NewObject()
		}
		backend := ""
		if b := selected.Obj("backend").Get("backend_id"); b != nil {
			backend = jsString(b)
		}
		strength := ""
		if p.Has("strength") {
			strength = " strength " + js(p, "strength")
		}
		where := "running jobs survive closing this dashboard"
		if location := OutputLocation(selected); location != "" {
			where = fmt.Sprintf("output %s (export: media-runtime outputs export %s --directory DIR)", location, js(selected, "job_id"))
		} else if c.Terminal(selected.Str("state")) {
			where = "no output"
		}
		if pool && selected.Str("worker_id") != "" {
			backend = selected.Str("worker_id") + " " + backend
		}
		if p.Has("frames") {
			strength += " frames " + js(p, "frames") + " at " + js(p, "fps") + " fps"
		}
		detail = fmt.Sprintf("%s  %s  %s×%s steps %s%s  %s", js(selected, "job_id"), backend, js(p, "width"), js(p, "height"), js(p, "steps"), strength, where)
	}
	lines = append(lines, pad(detail, columns))
	footer := state.Message
	if state.Error != "" {
		footer = "error: " + state.Error
	}
	lines = append(lines, pad(footer, columns))
	return lines
}

func clock() string { return time.Now().UTC().Format("15:04:05") + "Z" }

// Fetch refreshes the runtime summary and the newest jobs, keeping the selection on its job.
func Fetch(cl *client.Client, previous State) State {
	var health, listing *jsonv.Object
	var healthErr, listErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); health, healthErr = cl.Health() }()
	go func() { defer wg.Done(); listing, listErr = cl.Jobs(100, 0, true) }()
	wg.Wait()
	if err := firstErr(healthErr, listErr); err != nil {
		next := previous
		next.Error = "Runtime is unavailable"
		if e, ok := err.(*client.Error); ok {
			next.Error = e.Message
		}
		next.UpdatedAt = clock()
		return next
	}
	jobs := []*jsonv.Object{}
	for _, raw := range listing.Arr("jobs") {
		if j, ok := raw.(*jsonv.Object); ok {
			jobs = append(jobs, j)
		}
	}
	current := ""
	if previous.Selected >= 0 && previous.Selected < len(previous.Jobs) {
		current = previous.Jobs[previous.Selected].Str("job_id")
	}
	selected := -1
	for i, j := range jobs {
		if current != "" && j.Str("job_id") == current {
			selected = i
			break
		}
	}
	if selected < 0 {
		selected = previous.Selected
		if max := len(jobs) - 1; selected > max {
			selected = max
		}
		if selected < 0 {
			selected = 0
		}
	}
	return State{Runtime: health, Jobs: jobs, Selected: selected, Message: previous.Message, UpdatedAt: clock()}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Run draws the dashboard until q or Ctrl-C; closing it never cancels a job.
func Run(cl *client.Client, stdin *os.File, stdout io.Writer, interval time.Duration) error {
	var mu sync.Mutex
	state := State{}
	draw := func() {
		columns, rows := terminalSize(stdout)
		lines := Render(state, columns, rows)
		fmt.Fprint(stdout, "\x1b[H\x1b[2J"+strings.Join(lines, "\n")+"\n")
	}
	refresh := func() {
		next := Fetch(cl, func() State { mu.Lock(); defer mu.Unlock(); return state }())
		mu.Lock()
		state = next
		draw()
		mu.Unlock()
	}
	restore := makeRaw(stdin)
	defer restore()
	fmt.Fprint(stdout, "\x1b[?25l")
	refresh()
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	keys := make(chan string)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- string(buf[:n])
		}
	}()
	for key := range keys {
		if key == "q" || key == "\x03" {
			break
		}
		mu.Lock()
		switch key {
		case "j", "\x1b[B":
			state.Selected++
			if max := len(state.Jobs) - 1; state.Selected > max {
				state.Selected = max
			}
			if state.Selected < 0 {
				state.Selected = 0
			}
		case "k", "\x1b[A":
			if state.Selected > 0 {
				state.Selected--
			}
		}
		var cancel *jsonv.Object
		if key == "c" && state.Selected < len(state.Jobs) && !c.Terminal(state.Jobs[state.Selected].Str("state")) {
			cancel = state.Jobs[state.Selected]
		}
		mu.Unlock()
		if key == "r" {
			refresh()
			continue
		}
		if cancel != nil {
			_, err := cl.Cancel(cancel.Str("job_id"))
			mu.Lock()
			if err != nil {
				state.Error = err.Error()
			} else {
				state.Message = "cancellation requested for " + cancel.Str("job_id")
			}
			mu.Unlock()
			refresh()
			continue
		}
		mu.Lock()
		draw()
		mu.Unlock()
	}
	close(stop)
	fmt.Fprint(stdout, "\x1b[?25h\x1b[2J\x1b[H")
	return nil
}
