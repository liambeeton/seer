# Go, shipped as a single static binary

seer gets dropped onto engagement boxes and jump hosts where installing a runtime is not an option, so it is written in Go and every release is one statically linked binary per OS/arch. We accepted the cost of hand-plumbing CDP `Network.*` events to build the Response, instead of using Playwright (Node/Python), whose ergonomics are better but whose runtime would have to be installed alongside the tool.

## Consequences

- No CGO anywhere: the SQLite driver is `modernc.org/sqlite`, not `mattn/go-sqlite3`, so cross-compiling stays `GOOS=… GOARCH=… go build`.
- No Node in the build: the Report UI is `html/template` plus vanilla JS, embedded with `embed`.
- Chromium is the one external dependency. `--browser-path`, system lookup, rod's download (refusable with `--no-download`), and a Docker image with Chromium baked in cover the ways a box may or may not have one.
