package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/liambeeton/seer/internal/capture"
	"github.com/liambeeton/seer/internal/store"
	"github.com/liambeeton/seer/internal/target"
)

// captureOptions are the capture verb's flags.
type captureOptions struct {
	storeDir    string
	jsonl       bool
	browserPath string
	noDownload  bool
}

func newCaptureCmd() *cobra.Command {
	var opts captureOptions
	cmd := &cobra.Command{
		Use:   "capture <url>...",
		Short: "Visit each Target once and record a Capture into a Store",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCapture(cmd, opts, args)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&opts.storeDir, "store", "o", "./seer-store", "Store directory (created on first use)")
	f.BoolVar(&opts.jsonl, "jsonl", false, "write one JSON object per Capture to stdout as it completes")
	f.StringVar(&opts.browserPath, "browser-path", "", "Chrome/Chromium binary to use instead of looking one up")
	f.BoolVar(&opts.noDownload, "no-download", false, "fail instead of downloading a browser when none is found")
	return cmd
}

func runCapture(cmd *cobra.Command, opts captureOptions, urls []string) error {
	ctx := cmd.Context()
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()

	targets, err := target.Parse(urls)
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	browser, err := capture.Launch(ctx, capture.BrowserOptions{
		Path:       opts.browserPath,
		NoDownload: opts.noDownload,
		Log:        stderr,
	})
	if err != nil {
		return exitWith(ExitBrowserUnavailable, err)
	}
	defer browser.Close()

	st, err := store.Open(ctx, opts.storeDir)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	defer st.Close()

	run, err := st.CreateRun(ctx, len(targets))
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	done := 0
	for i, t := range targets {
		captured, err := browser.Visit(ctx, t.URL)
		if err != nil {
			return abort(st, run.ID, err)
		}
		stored, err := st.AppendCapture(ctx, store.Capture{
			RunID:      run.ID,
			Position:   i + 1,
			Target:     t.URL,
			Status:     captureStatus(captured),
			Error:      captured.Error,
			Response:   storeResponse(captured.Response),
			StartedAt:  captured.StartedAt,
			FinishedAt: captured.FinishedAt,
		}, captured.Screenshot)
		if err != nil {
			return abort(st, run.ID, err)
		}
		done++

		fmt.Fprintf(stderr, "[%d/%d] %s\n", done, len(targets), progress(stored))
		if opts.jsonl {
			if err := writeJSONL(stdout, stored); err != nil {
				return abort(st, run.ID, err)
			}
		}
	}

	if err := st.FinishRun(ctx, run.ID, store.RunCompleted); err != nil {
		return abort(st, run.ID, err)
	}
	return nil
}

// abort marks the Run aborted (best effort: the same fault may stop that too)
// and reports the cause.
func abort(st *store.Store, runID string, cause error) error {
	// The Run's own context may be the thing that is done, so use a fresh one.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.FinishRun(ctx, runID, store.RunAborted); err != nil {
		return exitWith(ExitAborted, fmt.Errorf("%v (and marking the Run aborted failed: %v)", cause, err))
	}
	return exitWith(ExitAborted, cause)
}

func captureStatus(c capture.Capture) store.CaptureStatus {
	if c.Succeeded() {
		return store.CaptureSucceeded
	}
	return store.CaptureFailed
}

func storeResponse(r *capture.Response) *store.Response {
	if r == nil {
		return nil
	}
	return &store.Response{StatusCode: r.StatusCode, FinalURL: r.FinalURL, Title: r.Title}
}

// progress is the "<status or FAILED> <target>" part of a progress line.
func progress(c store.Capture) string {
	switch {
	case c.Status == store.CaptureFailed:
		return fmt.Sprintf("FAILED %s (%s)", c.Target, c.Error)
	case c.Response == nil:
		return fmt.Sprintf("succeeded %s", c.Target)
	default:
		return fmt.Sprintf("%d %s", c.Response.StatusCode, c.Target)
	}
}

// jsonlCapture is the --jsonl shape of a Capture: the row, with nulls where
// the row has them.
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

func writeJSONL(w io.Writer, c store.Capture) error {
	out := jsonlCapture{
		ID:             c.ID,
		RunID:          c.RunID,
		Position:       c.Position,
		Target:         c.Target,
		Status:         string(c.Status),
		Error:          nullable(c.Error),
		ScreenshotPath: nullable(c.ScreenshotPath),
		StartedAt:      store.FormatTime(c.StartedAt),
		FinishedAt:     store.FormatTime(c.FinishedAt),
	}
	if c.Response != nil {
		out.Response = &jsonlResponse{Status: c.Response.StatusCode, FinalURL: c.Response.FinalURL, Title: c.Response.Title}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
