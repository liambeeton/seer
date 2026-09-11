package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/liambeeton/seer/internal/capture"
	"github.com/liambeeton/seer/internal/store"
	"github.com/liambeeton/seer/internal/target"
)

// captureOptions are the capture verb's flags.
type captureOptions struct {
	targetFile  string
	storeDir    string
	jsonl       bool
	browserPath string
	noDownload  bool
}

func newCaptureCmd() *cobra.Command {
	var opts captureOptions
	cmd := &cobra.Command{
		Use:   "capture [url]...",
		Short: "Visit each Target once and record a Capture into a Store",
		Long: "Visit each Target once and record a Capture into a Store.\n\n" +
			"Targets are read from the URLs given as arguments, from -f, or from\n" +
			"stdin when neither is given. A bare host becomes both an http:// and\n" +
			"an https:// Target; a line that names a scheme is visited as written.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCapture(cmd, opts, args)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&opts.targetFile, "file", "f", "", "file to read Targets from, one per line")
	f.StringVarP(&opts.storeDir, "store", "o", "./seer-store", "Store directory (created on first use)")
	f.BoolVar(&opts.jsonl, "jsonl", false, "write one JSON object per Capture to stdout as it completes")
	f.StringVar(&opts.browserPath, "browser-path", "", "Chrome/Chromium binary to use instead of looking one up")
	f.BoolVar(&opts.noDownload, "no-download", false, "fail instead of downloading a browser when none is found")
	return cmd
}

// targetLines gathers the input lines to parse: the URLs given as arguments
// and the lines of -f, or stdin when neither was given. Whichever it reads,
// nothing here judges a line; target.Parse does that.
func targetLines(stdin io.Reader, urls []string, file string) ([]string, error) {
	lines := append([]string(nil), urls...)

	var src io.Reader
	name := "stdin"
	switch {
	case file != "":
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("reading Targets: %w", err)
		}
		defer f.Close()
		src, name = f, file
	case len(urls) == 0:
		src = stdin
	default:
		return lines, nil
	}

	read, err := readLines(src)
	if err != nil {
		return nil, fmt.Errorf("reading Targets from %s: %w", name, err)
	}
	return append(lines, read...), nil
}

// readLines reads a Target list line by line. bufio.Scanner's default 64 KiB
// line limit is left in place: a longer line is a file that is not a Target
// list, and saying so beats reading it into memory.
func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func runCapture(cmd *cobra.Command, opts captureOptions, urls []string) error {
	ctx := cmd.Context()
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()

	lines, err := targetLines(cmd.InOrStdin(), urls, opts.targetFile)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	targets, err := target.Parse(lines)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	if len(targets) == 0 {
		return exitWith(ExitUsage, errors.New("no Targets given; pass URLs as arguments, with -f, or on stdin"))
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
	for _, t := range targets {
		captured, err := browser.Visit(ctx, t.URL)
		if err != nil {
			return abort(st, run.ID, err)
		}
		stored, err := st.AppendCapture(ctx, store.Capture{
			RunID:      run.ID,
			Position:   t.Position,
			Target:     t.URL,
			InputLine:  t.InputLine,
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
	out := &store.Response{
		StatusCode: r.StatusCode,
		FinalURL:   r.FinalURL,
		Title:      r.Title,
	}
	for _, hop := range r.RedirectChain {
		out.RedirectChain = append(out.RedirectChain, store.Hop{URL: hop.URL, Status: hop.Status})
	}
	for _, h := range r.Headers {
		out.Headers = append(out.Headers, store.Header{Name: h.Name, Value: h.Value})
	}
	if r.TLS != nil {
		out.TLS = &store.TLS{
			Subject:   r.TLS.Subject,
			SANs:      r.TLS.SANs,
			Issuer:    r.TLS.Issuer,
			ValidFrom: store.Timestamp(r.TLS.ValidFrom),
			ValidTo:   store.Timestamp(r.TLS.ValidTo),
			Protocol:  r.TLS.Protocol,
			Cipher:    r.TLS.Cipher,
			Trusted:   r.TLS.Trusted,
			Reason:    r.TLS.Reason,
		}
	}
	return out
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
// the row has them. The redirect chain, headers and TLS summary are the Store's
// own types, so a JSON line and the column it came from cannot drift apart.
type jsonlCapture struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id"`
	Position       int            `json:"position"`
	Target         string         `json:"target"`
	InputLine      string         `json:"input_line"`
	Status         string         `json:"status"`
	Error          *string        `json:"error"`
	Response       *jsonlResponse `json:"response"`
	ScreenshotPath *string        `json:"screenshot_path"`
	StartedAt      string         `json:"started_at"`
	FinishedAt     string         `json:"finished_at"`
}

type jsonlResponse struct {
	Status        int            `json:"status"`
	FinalURL      string         `json:"final_url"`
	Title         string         `json:"title"`
	RedirectChain []store.Hop    `json:"redirect_chain"`
	Headers       []store.Header `json:"headers"`
	TLS           *store.TLS     `json:"tls"`
}

func writeJSONL(w io.Writer, c store.Capture) error {
	out := jsonlCapture{
		ID:             c.ID,
		RunID:          c.RunID,
		Position:       c.Position,
		Target:         c.Target,
		InputLine:      c.InputLine,
		Status:         string(c.Status),
		Error:          nullable(c.Error),
		ScreenshotPath: nullable(c.ScreenshotPath),
		StartedAt:      store.FormatTime(c.StartedAt),
		FinishedAt:     store.FormatTime(c.FinishedAt),
	}
	if c.Response != nil {
		out.Response = &jsonlResponse{
			Status:        c.Response.StatusCode,
			FinalURL:      c.Response.FinalURL,
			Title:         c.Response.Title,
			RedirectChain: c.Response.RedirectChain,
			Headers:       c.Response.Headers,
			TLS:           c.Response.TLS,
		}
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
