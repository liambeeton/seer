// Package target turns input lines into Targets. It is pure: no I/O.
package target

import (
	"fmt"
	"net/url"
)

// Target is a single URL seer is asked to visit.
type Target struct {
	URL string
}

// Parse turns input lines into Targets in input order. Only URLs that already
// carry an http or https scheme are accepted; the first offending line is
// reported and nothing is returned, so a bad line never costs a visit.
func Parse(lines []string) ([]Target, error) {
	targets := make([]Target, 0, len(lines))
	for _, line := range lines {
		u, err := url.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("invalid target %q: %w", line, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("invalid target %q: URL must start with http:// or https://", line)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("invalid target %q: missing host", line)
		}
		targets = append(targets, Target{URL: line})
	}
	return targets, nil
}
