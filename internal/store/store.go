// Package store owns SQLite and the image files. A Store is a directory:
// seer.db plus screenshots/<capture-id>.jpg.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO
)

const (
	dbFile         = "seer.db"
	screenshotsDir = "screenshots"
)

// TimeLayout is how the Store writes every timestamp: UTC RFC 3339 at a
// fixed millisecond width, so the text sorts chronologically.
const TimeLayout = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t the way the Store stores it.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// RunStatus is the lifecycle state of a Run.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunAborted   RunStatus = "aborted"
)

// CaptureStatus says whether the browser produced a document.
type CaptureStatus string

const (
	CaptureSucceeded CaptureStatus = "succeeded"
	CaptureFailed    CaptureStatus = "failed"
)

// Run is one invocation of seer over a set of Targets.
type Run struct {
	ID          string
	StartedAt   time.Time
	FinishedAt  time.Time // zero while the Run is running
	Status      RunStatus
	TargetCount int
}

// Timestamp is a time the Store keeps inside a JSON column, written the way
// the Store writes every other timestamp so that a time in JSON looks like a
// time in a column of its own.
type Timestamp time.Time

// MarshalJSON writes the timestamp in the Store's layout.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(FormatTime(time.Time(t)))
}

// Hop is one step of a redirect chain: the URL that was requested and the
// status it answered with.
type Hop struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// Header is one response header, exactly as it came over the wire.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TLS is the certificate the Target served and the browser's verdict on it.
type TLS struct {
	Subject   string    `json:"subject"`
	SANs      []string  `json:"sans"`
	Issuer    string    `json:"issuer"`
	ValidFrom Timestamp `json:"valid_from"`
	ValidTo   Timestamp `json:"valid_to"`
	Protocol  string    `json:"protocol"`
	Cipher    string    `json:"cipher"`
	// Trusted is whether the browser accepted the certificate.
	Trusted bool `json:"trusted"`
	// Reason is why it did not; empty when it did.
	Reason string `json:"reason"`
}

// Response is what the Target's server returned during a Capture's visit.
type Response struct {
	StatusCode int
	FinalURL   string
	Title      string
	// RedirectChain is every hop the visit took, in order, the final one
	// included.
	RedirectChain []Hop
	// Headers are the final hop's response headers, verbatim.
	Headers []Header
	// TLS is nil when the final hop was not served over TLS.
	TLS *TLS
}

// Capture is the record of one visit to one Target in one Run.
type Capture struct {
	ID       string
	RunID    string
	Position int // visit order, starting at 1
	Target   string
	// InputLine is the input line the Target was expanded from, kept so a
	// Report can explain why one line became two Captures.
	InputLine string
	Status    CaptureStatus
	Error     string    // failure reason; empty when succeeded
	Response  *Response // nil when nothing came back
	// ScreenshotPath is relative to the Store, slash-separated; empty when
	// there is no Screenshot.
	ScreenshotPath string
	StartedAt      time.Time
	FinishedAt     time.Time
}

// Store is an open Store directory.
type Store struct {
	dir string
	db  *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id           TEXT PRIMARY KEY,
	started_at   TEXT NOT NULL,
	finished_at  TEXT,
	status       TEXT NOT NULL CHECK (status IN ('running', 'completed', 'aborted')),
	target_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS captures (
	id              TEXT PRIMARY KEY,
	run_id          TEXT NOT NULL REFERENCES runs(id),
	position        INTEGER NOT NULL,
	target_url      TEXT NOT NULL,
	input_line      TEXT NOT NULL,
	status          TEXT NOT NULL CHECK (status IN ('succeeded', 'failed')),
	error           TEXT,
	http_status     INTEGER,
	final_url       TEXT,
	title           TEXT,
	headers         TEXT,
	redirect_chain  TEXT,
	tls             TEXT,
	screenshot_path TEXT,
	started_at      TEXT NOT NULL,
	finished_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS captures_run_position ON captures (run_id, position);
CREATE INDEX IF NOT EXISTS captures_target_url ON captures (target_url);
`

// Open opens the Store at dir, creating the directory, the database and the
// schema on first use. The database runs in WAL mode so a Report can be
// served while a Run is still writing.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, screenshotsDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating Store: %w", err)
	}
	dsn := "file:" + filepath.Join(dir, dbFile) + "?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening Store database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("preparing Store database: %w", err)
	}
	if err := addLaterColumns(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{dir: dir, db: db}, nil
}

// laterCaptureColumns are the nullable captures columns added after the table
// was first written, newest last. A Store opened for the first time gets them
// from schema; one written by an earlier seer gets them from addLaterColumns.
// Add to both when a ticket adds a column.
var laterCaptureColumns = []struct{ name, kind string }{
	{"headers", "TEXT"},
	{"redirect_chain", "TEXT"},
	{"tls", "TEXT"},
}

// addLaterColumns brings a Store written by an earlier seer up to the current
// schema. CREATE TABLE IF NOT EXISTS leaves an existing table exactly as it
// found it, so without this an older Store would reject every Capture written
// to it and take the whole Run down with it.
func addLaterColumns(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('captures')`)
	if err != nil {
		return fmt.Errorf("reading the Store's schema: %w", err)
	}
	defer rows.Close()
	present := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("reading the Store's schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading the Store's schema: %w", err)
	}

	for _, column := range laterCaptureColumns {
		if present[column.name] {
			continue
		}
		// The names are this file's own, never user input.
		if _, err := db.ExecContext(ctx, "ALTER TABLE captures ADD COLUMN "+column.name+" "+column.kind); err != nil {
			return fmt.Errorf("adding the %s column to the Store: %w", column.name, err)
		}
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateRun records a new running Run and returns it.
func (s *Store) CreateRun(ctx context.Context, targetCount int) (Run, error) {
	run := Run{
		ID:          newID(),
		StartedAt:   now(),
		Status:      RunRunning,
		TargetCount: targetCount,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, started_at, status, target_count) VALUES (?, ?, ?, ?)`,
		run.ID, FormatTime(run.StartedAt), run.Status, run.TargetCount)
	if err != nil {
		return Run{}, fmt.Errorf("creating Run: %w", err)
	}
	return run, nil
}

// FinishRun marks the Run completed or aborted and stamps its finish time.
func (s *Store) FinishRun(ctx context.Context, id string, status RunStatus) error {
	if status == RunRunning {
		return errors.New("a Run cannot finish as running")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
		status, FormatTime(now()), id)
	if err != nil {
		return fmt.Errorf("finishing Run %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finishing Run %s: no such Run", id)
	}
	return nil
}

// AppendCapture writes the Screenshot (if any) as an image file and inserts
// the Capture row. The Store assigns the id and the screenshot path; the
// returned Capture carries both.
func (s *Store) AppendCapture(ctx context.Context, c Capture, screenshot []byte) (Capture, error) {
	c.ID = newID()
	c.ScreenshotPath = ""
	if screenshot != nil {
		rel := path.Join(screenshotsDir, c.ID+".jpg")
		if err := os.WriteFile(filepath.Join(s.dir, filepath.FromSlash(rel)), screenshot, 0o644); err != nil {
			return Capture{}, fmt.Errorf("writing Screenshot: %w", err)
		}
		c.ScreenshotPath = rel
	}

	var httpStatus, finalURL, title any
	if c.Response != nil {
		httpStatus, finalURL, title = c.Response.StatusCode, c.Response.FinalURL, c.Response.Title
	}
	headers, redirectChain, tlsSummary, err := responseColumns(c.Response)
	if err != nil {
		return Capture{}, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO captures
			(id, run_id, position, target_url, input_line, status, error, http_status, final_url, title, headers, redirect_chain, tls, screenshot_path, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.RunID, c.Position, c.Target, c.InputLine, c.Status, nullIfEmpty(c.Error),
		httpStatus, finalURL, title, headers, redirectChain, tlsSummary, nullIfEmpty(c.ScreenshotPath),
		FormatTime(c.StartedAt), FormatTime(c.FinishedAt))
	if err != nil {
		if c.ScreenshotPath != "" {
			_ = os.Remove(filepath.Join(s.dir, filepath.FromSlash(c.ScreenshotPath)))
		}
		return Capture{}, fmt.Errorf("recording Capture: %w", err)
	}
	return c, nil
}

// responseColumns renders the parts of a Response the Store keeps as JSON.
// Each is NULL when the visit observed nothing to keep.
func responseColumns(r *Response) (headers, redirectChain, tlsSummary any, err error) {
	if r == nil {
		return nil, nil, nil, nil
	}
	if len(r.Headers) > 0 {
		if headers, err = encodeJSON(r.Headers); err != nil {
			return nil, nil, nil, err
		}
	}
	if len(r.RedirectChain) > 0 {
		if redirectChain, err = encodeJSON(r.RedirectChain); err != nil {
			return nil, nil, nil, err
		}
	}
	if r.TLS != nil {
		if tlsSummary, err = encodeJSON(r.TLS); err != nil {
			return nil, nil, nil, err
		}
	}
	return headers, redirectChain, tlsSummary, nil
}

func encodeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding the Response: %w", err)
	}
	return string(b), nil
}

func newID() string {
	return ulid.Make().String()
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
