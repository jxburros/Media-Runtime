// Package integration holds the cross-process tests that drive the real Media Engine HTTP
// worker through its explicitly identified test fixture (tests/serve_fixture.py), and the
// helpers other packages' tests use to launch it.
package integration

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

const WorkerToken = "worker-test-token-not-a-secret-000000000000"

// EngineRepo is $MEDIA_ENGINE_REPO or the Media-Engine checkout beside this one.
func EngineRepo(t testing.TB) string {
	t.Helper()
	repo := os.Getenv("MEDIA_ENGINE_REPO")
	if repo == "" {
		_, here, _, _ := callerFile()
		repo = filepath.Join(filepath.Dir(here), "..", "..", "..", "Media-Engine")
	}
	abs, _ := filepath.Abs(repo)
	if _, err := os.Stat(filepath.Join(abs, "tests", "serve_fixture.py")); err != nil {
		t.Skipf("Media Engine fixture not found at %s; set MEDIA_ENGINE_REPO", abs)
	}
	return abs
}

// Python is $MEDIA_ENGINE_PYTHON, else python3, else python.
func Python() string {
	if p := os.Getenv("MEDIA_ENGINE_PYTHON"); p != "" {
		return p
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return "python"
}

// FixtureCommand launches Engine's fixture worker with its state under root.
func FixtureCommand(engine, root string, args ...string) []string {
	return append([]string{Python(), filepath.Join(engine, "tests", "serve_fixture.py"), root}, args...)
}

// Fixture is an externally managed fixture worker.
type Fixture struct {
	URL       string
	Discovery *jsonv.Object
	cmd       *exec.Cmd
}

// StartFixture launches the fixture worker and stops it when the test ends.
func StartFixture(t testing.TB, root string, args ...string) *Fixture {
	t.Helper()
	engine := EngineRepo(t)
	command := FixtureCommand(engine, root, args...)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(engine, "src"), "MEDIA_ENGINE_TOKEN="+WorkerToken)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		line, _ := reader.ReadString('\n')
		lines <- line
		_, _ = reader.WriteTo(discard{})
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(30 * time.Second):
	}
	parsed, err := jsonv.Parse([]byte(strings.TrimSpace(line)))
	discovery, _ := parsed.(*jsonv.Object)
	if err != nil || discovery == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("fixture did not start: %s", stderr.String())
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	port, _ := discovery.Num("port")
	return &Fixture{URL: fmt.Sprintf("http://127.0.0.1:%d", int(port)), Discovery: discovery, cmd: cmd}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// MakePNG builds an 8-bit RGB PNG of a flat colour.
func MakePNG(width, height int) []byte {
	chunk := func(kind string, data []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		body := append([]byte(kind), data...)
		b.Write(body)
		_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(body))
		return b.Bytes()
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8], ihdr[9] = 8, 2
	var raw bytes.Buffer
	row := append([]byte{0}, bytes.Repeat([]byte{0x40}, width*3)...)
	for i := 0; i < height; i++ {
		raw.Write(row)
	}
	var idat bytes.Buffer
	z := zlib.NewWriter(&idat)
	_, _ = z.Write(raw.Bytes())
	_ = z.Close()
	var png bytes.Buffer
	png.WriteString("\x89PNG\r\n\x1a\n")
	png.Write(chunk("IHDR", ihdr))
	png.Write(chunk("IDAT", idat.Bytes()))
	png.Write(chunk("IEND", nil))
	return png.Bytes()
}

// newRequest performs an unauthenticated GET and returns the status.
func newRequest(url string, headers map[string]string) (int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}
