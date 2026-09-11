package capture

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Visit parameters. Later tickets turn these into options.
const (
	navigationTimeout = 30 * time.Second
	settle            = time.Second
	viewportWidth     = 1440
	viewportHeight    = 900
	jpegQuality       = 85
	closeTimeout      = 5 * time.Second
)

// Response is what the Target's server returned during the visit.
type Response struct {
	StatusCode int
	FinalURL   string
	Title      string
}

// Capture is the record of one visit to one Target: succeeded when the browser
// produced a document (any status code), failed when it could not reach one.
type Capture struct {
	Target string
	// Error is why the browser produced no document; empty when it did.
	Error string
	// Response is nil when nothing came back from the server at all.
	Response *Response
	// Screenshot holds the viewport JPEG; nil when the visit failed.
	Screenshot []byte
	StartedAt  time.Time
	FinishedAt time.Time
}

// Succeeded reports whether the browser produced a document.
func (c Capture) Succeeded() bool { return c.Error == "" }

// Visit navigates to the Target in a fresh incognito context and returns its
// Capture. A Target that cannot be reached yields a failed Capture, not an
// error; the error return is reserved for the Run being stopped through ctx.
func (b *Browser) Visit(ctx context.Context, url string) (Capture, error) {
	c := Capture{Target: url, StartedAt: now()}
	resp, screenshot, err := b.visit(ctx, url)
	c.FinishedAt = now()
	c.Response = resp
	if err != nil {
		if ctx.Err() != nil {
			return Capture{}, ctx.Err()
		}
		// A visit error is the Target's fault only while the browser itself
		// is still there to blame it on.
		if err := b.ping(ctx); err != nil {
			return Capture{}, fmt.Errorf("browser unavailable: %w", err)
		}
		c.Error = reason(err)
		return c, nil
	}
	c.Screenshot = screenshot
	return c, nil
}

// ping fails when the browser no longer answers.
func (b *Browser) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()
	_, err := proto.BrowserGetVersion{}.Call(b.rod.Context(ctx))
	return err
}

func (b *Browser) visit(ctx context.Context, url string) (*Response, []byte, error) {
	incognito, err := b.rod.Context(ctx).Incognito()
	if err != nil {
		return nil, nil, fmt.Errorf("creating browser context: %w", err)
	}
	defer func() { _ = incognito.Close() }()

	page, err := incognito.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, nil, fmt.Errorf("opening page: %w", err)
	}
	defer func() { _ = page.Timeout(closeTimeout).Close() }()

	err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             viewportWidth,
		Height:            viewportHeight,
		DeviceScaleFactor: 1,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("setting viewport: %w", err)
	}

	doc := watchDocument(page)
	defer doc.stop()

	nav := page.Timeout(navigationTimeout)
	if err := nav.Navigate(url); err != nil {
		return doc.response(), nil, err
	}
	if err := nav.WaitLoad(); err != nil {
		return doc.response(), nil, err
	}

	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return doc.response(), nil, ctx.Err()
	}

	after := page.Timeout(navigationTimeout)
	title, err := after.Eval(`() => document.title`)
	if err != nil {
		return doc.response(), nil, fmt.Errorf("reading title: %w", err)
	}
	quality := jpegQuality
	screenshot, err := after.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:  proto.PageCaptureScreenshotFormatJpeg,
		Quality: &quality,
	})
	if err != nil {
		return doc.response(), nil, fmt.Errorf("taking Screenshot: %w", err)
	}

	resp := doc.response()
	if resp == nil {
		resp = &Response{}
	}
	resp.Title = title.Value.Str()
	if resp.FinalURL == "" {
		if info, err := after.Info(); err == nil {
			resp.FinalURL = info.URL
		}
	}
	return resp, screenshot, nil
}

// documentWatcher records the main frame's latest document response, which is
// the Response's status code and final URL.
type documentWatcher struct {
	mu   sync.Mutex
	last *proto.NetworkResponse
	stop func()
}

func watchDocument(page *rod.Page) *documentWatcher {
	w := &documentWatcher{}
	events, cancel := page.WithCancel()
	wait := events.EachEvent(func(e *proto.NetworkResponseReceived) {
		if e.Type != proto.NetworkResourceTypeDocument || e.FrameID != page.FrameID {
			return
		}
		w.mu.Lock()
		w.last = e.Response
		w.mu.Unlock()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	w.stop = func() {
		cancel()
		<-done
	}
	return w
}

func (w *documentWatcher) response() *Response {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		return nil
	}
	return &Response{StatusCode: w.last.Status, FinalURL: w.last.URL}
}

// reason turns a visit error into the failure reason recorded on the Capture.
func reason(err error) string {
	var nav *rod.NavigationError
	if errors.As(err, &nav) {
		return nav.Reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s", navigationTimeout)
	}
	return err.Error()
}

// now is the visit clock: UTC at millisecond precision, which is what the
// Store keeps and the JSONL streams.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}
