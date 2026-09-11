// Package capture owns the browser. It launches one per Run and visits one
// Target at a time in a fresh context, returning a Capture. It never touches
// SQL or the filesystem beyond the image bytes it returns.
package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// BrowserOptions say how a browser is provisioned.
type BrowserOptions struct {
	// Path is an explicit Chrome/Chromium binary. Empty means look one up on
	// the system and, failing that, download one.
	Path string
	// NoDownload turns the download step into an error.
	NoDownload bool
	// Log receives download progress. Nil discards it.
	Log io.Writer
}

// Browser is a running headless Chromium that visits Targets.
type Browser struct {
	launcher *launcher.Launcher
	rod      *rod.Browser
}

// Launch provisions a browser binary (explicit path, then system lookup, then
// download) and starts it headless. Every error means the browser is
// unavailable.
func Launch(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	bin, err := provision(ctx, opts)
	if err != nil {
		return nil, err
	}

	l := launcher.New().Context(ctx).Bin(bin).Headless(true)
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launching browser %s: %w", bin, err)
	}

	b := rod.New().Context(ctx).ControlURL(controlURL).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("connecting to browser %s: %w", bin, err)
	}
	return &Browser{launcher: l, rod: b}, nil
}

func provision(ctx context.Context, opts BrowserOptions) (string, error) {
	if opts.Path != "" {
		if _, err := os.Stat(opts.Path); err != nil {
			return "", fmt.Errorf("browser not found at %s", opts.Path)
		}
		return opts.Path, nil
	}
	if bin, found := launcher.LookPath(); found {
		return bin, nil
	}
	if opts.NoDownload {
		return "", errors.New("no browser found on this system and downloading is disabled (--no-download); use --browser-path")
	}

	dl := launcher.NewBrowser()
	dl.Context = ctx
	logOut := opts.Log
	if logOut == nil {
		logOut = io.Discard
	}
	dl.Logger = log.New(logOut, "", 0)
	bin, err := dl.Get()
	if err != nil {
		return "", fmt.Errorf("downloading browser: %w", err)
	}
	return bin, nil
}

// Close asks the browser to exit, kills it if it has not within a few
// seconds, and removes its temporary profile.
func (b *Browser) Close() error {
	err := b.rod.Close()
	exited := make(chan struct{})
	go func() {
		b.launcher.Cleanup() // waits for the process to exit, then removes the profile
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(closeTimeout):
		b.launcher.Kill()
		<-exited
	}
	return err
}
