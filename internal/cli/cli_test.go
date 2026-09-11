package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liambeeton/seer/internal/cli"
)

// invocation is everything a user can observe from one seer invocation
// besides the Store's files.
type invocation struct {
	stdout, stderr string
	code           int
}

// seer runs one invocation through the cli.Run seam with an empty stdin.
func seer(t *testing.T, args ...string) invocation {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	t.Logf("seer %s\n  exit %d\n  stdout: %q\n  stderr: %q", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return invocation{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// browserFlags never lets a test download a browser. SEER_TEST_BROWSER points
// at a specific binary; otherwise the system lookup must find one.
func browserFlags() []string {
	flags := []string{"--no-download"}
	if path := os.Getenv("SEER_TEST_BROWSER"); path != "" {
		flags = append(flags, "--browser-path", path)
	}
	return flags
}

func assertNoStore(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Store %s exists (stat err %v); expected none to be created", dir, err)
	}
}

func TestCapture_NonexistentBrowserPathExits2WithoutStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	missing := filepath.Join(t.TempDir(), "no-such-chrome")

	r := seer(t, "capture", "https://example.test/", "-o", store, "--browser-path", missing)

	if r.code != 2 {
		t.Errorf("exit code = %d, want 2", r.code)
	}
	if !strings.Contains(r.stderr, missing) {
		t.Errorf("stderr should name the missing browser path %q, got %q", missing, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout should be empty, got %q", r.stdout)
	}
	assertNoStore(t, store)
}

// jsonlCapture is the documented shape of one --jsonl line.
type jsonlCapture struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id"`
	Position       int            `json:"position"`
	Target         string         `json:"target"`
	Status         string         `json:"status"`
	Error          *string        `json:"error"`
	Response       *jsonlResponse `json:"response"`
	ScreenshotPath *string        `json:"screenshot_path"`
	StartedAt      string         `json:"started_at"`
	FinishedAt     string         `json:"finished_at"`
}

type jsonlResponse struct {
	Status   int    `json:"status"`
	FinalURL string `json:"final_url"`
	Title    string `json:"title"`
}

// captures decodes stdout as JSONL, failing on anything that is not one JSON
// object per line.
func captures(t *testing.T, stdout string) []jsonlCapture {
	t.Helper()
	var out []jsonlCapture
	for i, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			if stdout == "" {
				return nil
			}
			t.Fatalf("stdout line %d is empty; stdout: %q", i+1, stdout)
		}
		var c jsonlCapture
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("stdout line %d is not a JSON object: %v; line: %q", i+1, err, line)
		}
		out = append(out, c)
	}
	return out
}

// fixture200 serves a page whose title is set by JavaScript, so a title read
// before the page settles would be wrong.
func fixture200(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>static</title></head>`+
			`<body><h1>Fixture</h1><script>document.title = "Fixture 200"</script></body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertUTCTimestamp(t *testing.T, name, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Errorf("%s = %q is not RFC 3339: %v", name, value, err)
	}
	if !strings.HasSuffix(value, "Z") {
		t.Errorf("%s = %q is not UTC", name, value)
	}
	return ts
}

func TestCapture_ReachableTargetYieldsSucceededCapture(t *testing.T) {
	srv := fixture200(t)
	target := srv.URL + "/"
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", target, "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 1 {
		t.Fatalf("got %d JSONL lines, want 1; stdout: %q", len(cs), r.stdout)
	}
	c := cs[0]

	if c.ID == "" || c.RunID == "" {
		t.Errorf("id %q and run_id %q must both be set", c.ID, c.RunID)
	}
	if c.Position != 1 {
		t.Errorf("position = %d, want 1", c.Position)
	}
	if c.Target != target {
		t.Errorf("target = %q, want %q", c.Target, target)
	}
	if c.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", c.Status)
	}
	if c.Error != nil {
		t.Errorf("error = %q, want null", *c.Error)
	}
	if c.Response == nil {
		t.Fatalf("response is null, want status/final_url/title")
	}
	if c.Response.Status != 200 {
		t.Errorf("response.status = %d, want 200", c.Response.Status)
	}
	if c.Response.FinalURL != target {
		t.Errorf("response.final_url = %q, want %q", c.Response.FinalURL, target)
	}
	if c.Response.Title != "Fixture 200" {
		t.Errorf("response.title = %q, want the JavaScript-set title %q", c.Response.Title, "Fixture 200")
	}
	if !strings.Contains(r.stderr, "[1/1] 200 "+target) {
		t.Errorf("stderr progress should read \"[1/1] 200 <target>\", got %q", r.stderr)
	}
	if c.ScreenshotPath == nil || *c.ScreenshotPath != "screenshots/"+c.ID+".jpg" {
		t.Fatalf("screenshot_path = %v, want screenshots/%s.jpg", c.ScreenshotPath, c.ID)
	}
	started := assertUTCTimestamp(t, "started_at", c.StartedAt)
	finished := assertUTCTimestamp(t, "finished_at", c.FinishedAt)
	if finished.Before(started) {
		t.Errorf("finished_at %s is before started_at %s", c.FinishedAt, c.StartedAt)
	}

	// The Store is a directory holding seer.db (in WAL mode: header bytes 18
	// and 19 are the write/read format versions, 2 for WAL) and the image.
	header := make([]byte, 20)
	db, err := os.Open(filepath.Join(store, "seer.db"))
	if err != nil {
		t.Fatalf("seer.db: %v", err)
	}
	defer db.Close()
	if _, err := io.ReadFull(db, header); err != nil {
		t.Fatalf("reading seer.db header: %v", err)
	}
	if header[18] != 2 || header[19] != 2 {
		t.Errorf("seer.db is not in WAL mode (format versions %d/%d, want 2/2)", header[18], header[19])
	}
	screenshot, err := os.Open(filepath.Join(store, filepath.FromSlash(*c.ScreenshotPath)))
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	defer screenshot.Close()
	cfg, format, err := image.DecodeConfig(screenshot)
	if err != nil {
		t.Fatalf("Screenshot is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("Screenshot format = %q, want jpeg", format)
	}
	if cfg.Width != 1440 || cfg.Height != 900 {
		t.Errorf("Screenshot is %dx%d, want 1440x900", cfg.Width, cfg.Height)
	}
}

// refusedTarget is a URL on a loopback port nothing is listening on.
func refusedTarget(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr + "/"
}

func TestCapture_RefusedConnectionYieldsFailedCapture(t *testing.T) {
	target := refusedTarget(t)
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", target, "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0 (a failed Capture still completes the Run); stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 1 {
		t.Fatalf("got %d JSONL lines, want 1; stdout: %q", len(cs), r.stdout)
	}
	c := cs[0]
	if c.Status != "failed" {
		t.Errorf("status = %q, want failed", c.Status)
	}
	if c.Target != target {
		t.Errorf("target = %q, want %q", c.Target, target)
	}
	if c.Error == nil || !strings.Contains(*c.Error, "CONNECTION_REFUSED") {
		t.Errorf("error = %v, want the refused-connection reason", c.Error)
	}
	if c.Response != nil {
		t.Errorf("response = %+v, want null: nothing answered", *c.Response)
	}
	if c.ScreenshotPath != nil {
		t.Errorf("screenshot_path = %q, want null", *c.ScreenshotPath)
	}
	assertUTCTimestamp(t, "started_at", c.StartedAt)
	assertUTCTimestamp(t, "finished_at", c.FinishedAt)

	screenshots, err := filepath.Glob(filepath.Join(store, "screenshots", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(screenshots) != 0 {
		t.Errorf("a failed Capture must not leave a Screenshot, found %v", screenshots)
	}
	if !strings.Contains(r.stderr, "[1/1] FAILED "+target) {
		t.Errorf("stderr progress should read \"[1/1] FAILED <target>\", got %q", r.stderr)
	}
}

func TestCapture_RunCompletesWhenSomeCapturesFail(t *testing.T) {
	srv := fixture200(t)
	reachable := srv.URL + "/"
	refused := refusedTarget(t)
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", reachable, refused, "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 2 {
		t.Fatalf("got %d JSONL lines, want one per Target (2); stdout: %q", len(cs), r.stdout)
	}
	if cs[0].Position != 1 || cs[0].Target != reachable || cs[0].Status != "succeeded" {
		t.Errorf("first line = position %d %s %s, want 1 %s succeeded", cs[0].Position, cs[0].Target, cs[0].Status, reachable)
	}
	if cs[1].Position != 2 || cs[1].Target != refused || cs[1].Status != "failed" {
		t.Errorf("second line = position %d %s %s, want 2 %s failed", cs[1].Position, cs[1].Target, cs[1].Status, refused)
	}
	if cs[0].RunID == "" || cs[0].RunID != cs[1].RunID {
		t.Errorf("run_id %q and %q should be the same Run", cs[0].RunID, cs[1].RunID)
	}
	if cs[0].ID == cs[1].ID {
		t.Errorf("both Captures have id %q", cs[0].ID)
	}
}

func TestCapture_WithoutJSONLStdoutStaysEmpty(t *testing.T) {
	srv := fixture200(t)
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", srv.URL + "/", "-o", store}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty without --jsonl", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(store, "seer.db")); err != nil {
		t.Errorf("the Capture should still be recorded: %v", err)
	}
	screenshots, _ := filepath.Glob(filepath.Join(store, "screenshots", "*.jpg"))
	if len(screenshots) != 1 {
		t.Errorf("want exactly one Screenshot, found %v", screenshots)
	}
}

func TestCapture_TargetWithoutSchemeIsRejectedBeforeAnyVisit(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", "https://ok.test/", "example.test", "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 1 {
		t.Errorf("exit code = %d, want 1", r.code)
	}
	if !strings.Contains(r.stderr, "example.test") {
		t.Errorf("stderr should name the offending line, got %q", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
	assertNoStore(t, store)
}

func TestCapture_HelpDescribesFlags(t *testing.T) {
	r := seer(t, "capture", "--help")

	if r.code != 0 {
		t.Errorf("exit code = %d, want 0", r.code)
	}
	for _, want := range []string{"-o", "./seer-store", "--jsonl", "--browser-path", "--no-download"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help should mention %q, got:\n%s", want, r.stdout)
		}
	}
}
