// Package store owns SQLite and the image files. A Store is a directory:
// seer.db plus screenshots/<capture-id>.jpg.
package store

import (
	"context"
	"database/sql"
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

// Response is what the Target's server returned during a Capture's visit.
type Response struct {
	StatusCode int
	FinalURL   string
	Title      string
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
	return &Store{dir: dir, db: db}, nil
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO captures
			(id, run_id, position, target_url, input_line, status, error, http_status, final_url, title, screenshot_path, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.RunID, c.Position, c.Target, c.InputLine, c.Status, nullIfEmpty(c.Error),
		httpStatus, finalURL, title, nullIfEmpty(c.ScreenshotPath),
		FormatTime(c.StartedAt), FormatTime(c.FinishedAt))
	if err != nil {
		if c.ScreenshotPath != "" {
			_ = os.Remove(filepath.Join(s.dir, filepath.FromSlash(c.ScreenshotPath)))
		}
		return Capture{}, fmt.Errorf("recording Capture: %w", err)
	}
	return c, nil
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
