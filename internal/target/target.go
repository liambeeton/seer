// Package target turns input lines into Targets. It is pure: no I/O.
package target

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Target is a single URL seer is asked to visit, carrying the provenance that
// explains why it is in the Run.
type Target struct {
	// URL is the normalised URL to visit.
	URL string
	// InputLine is the input line this Target was expanded from, with
	// surrounding whitespace removed. One line can yield two Targets.
	InputLine string
	// Position is the Target's place in the visit order, starting at 1.
	Position int
}

// defaultPorts are the ports a scheme implies, and which are therefore dropped
// during normalisation.
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// hasScheme matches a line that names its own scheme, such as
// "https://acme.com/". It is deliberately not url.Parse, which reads
// "acme.com:8080" as the scheme "acme.com".
var hasScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// Parse turns input lines into the Targets a Run visits, in visit order.
//
// Blank lines and surrounding whitespace are ignored. A line that names a
// scheme becomes one Target; a bare host or host:port becomes two, one per
// scheme. Lines that normalise to the same URL collapse to one Target and the
// first occurrence keeps its position. Any line that is not a URL, a host or a
// host:port is an error naming that line, so a typo never costs a visit.
func Parse(lines []string) ([]Target, error) {
	var targets []Target
	seen := make(map[string]bool)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		urls, err := expand(line)
		if err != nil {
			return nil, fmt.Errorf("invalid target %q: %w", line, err)
		}
		for _, u := range urls {
			if seen[u] {
				continue
			}
			seen[u] = true
			targets = append(targets, Target{URL: u, InputLine: line, Position: len(targets) + 1})
		}
	}
	return targets, nil
}

// expand turns one input line into the normalised URLs it stands for: the one
// it names, or one per scheme when it names only a host.
func expand(line string) ([]string, error) {
	if hasScheme.MatchString(line) {
		u, err := url.Parse(line)
		if err != nil {
			return nil, unwrapParseError(err)
		}
		if _, ok := defaultPorts[strings.ToLower(u.Scheme)]; !ok {
			return nil, fmt.Errorf("scheme %q is not one seer visits; use http:// or https://", u.Scheme)
		}
		if err := checkAuthority(u); err != nil {
			return nil, err
		}
		return []string{normalise(u)}, nil
	}

	// Only a host or host:port, optionally with a trailing slash, may go
	// without a scheme: a line like "10.0.0.0/24" would otherwise parse as a
	// host with a path, and seer expands no ranges.
	u, err := url.Parse("http://" + line)
	if err != nil {
		if ip := net.ParseIP(line); ip != nil && ip.To4() == nil {
			return nil, fmt.Errorf("an IPv6 literal must be written in brackets, as [%s]", line)
		}
		return nil, unwrapParseError(err)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		if _, _, err := net.ParseCIDR(line); err == nil {
			return nil, errors.New("seer expands no ranges; give each Target in the range its own line")
		}
		return nil, errors.New("only a host or host:port may go without a scheme; write the whole URL, http:// or https:// included, to visit a path")
	}
	if err := checkAuthority(u); err != nil {
		return nil, err
	}
	both := make([]string, 0, 2)
	for _, scheme := range []string{"http", "https"} {
		u.Scheme = scheme
		both = append(both, normalise(u))
	}
	return both, nil
}

// normalise lowercases the scheme and host, drops a port the scheme already
// implies and fills in a path of at least "/". Path, query and fragment are
// otherwise left exactly as written.
func normalise(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal keeps its brackets
	}
	if port := u.Port(); port != "" && port != defaultPorts[scheme] {
		host += ":" + port
	}

	// Built by hand rather than with url.URL.String, which percent-escapes the
	// non-ASCII host of an IDN Target into something no resolver would answer.
	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	if path := u.EscapedPath(); path != "" {
		b.WriteString(path)
	} else {
		b.WriteString("/")
	}
	if u.ForceQuery || u.RawQuery != "" {
		b.WriteString("?")
		b.WriteString(u.RawQuery)
	}
	if u.Fragment != "" {
		b.WriteString("#")
		b.WriteString(u.EscapedFragment())
	}
	return b.String()
}

// checkAuthority rejects what a Target's authority may not hold: credentials,
// a port outside the range, or a host no resolver could ever answer. Garbage is
// worth an input error rather than a Run's worth of failed Captures.
func checkAuthority(u *url.URL) error {
	if u.User != nil {
		return errors.New("seer visits no Target with credentials in its URL")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	if port := u.Port(); port != "" {
		// net/url has already checked that the port is all digits.
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port %s is not between 1 and 65535", port)
		}
	}
	if strings.HasPrefix(u.Host, "[") {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("%q is not an IPv6 address", host)
		}
		return nil
	}
	// A fully qualified host may end in a single dot; no other label may be
	// empty.
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if !hostLabel(label) {
			return fmt.Errorf("%q is not a host name", host)
		}
	}
	return nil
}

// hostLabel reports whether one dot-separated piece of a host name is one:
// letters, digits, underscores and inner hyphens, plus the non-ASCII runes an
// IDN host is written with.
func hostLabel(label string) bool {
	if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		case r >= 0x80: // an IDN host is written in its own script
		default:
			return false
		}
	}
	return true
}

// unwrapParseError strips net/url's wrapper, whose message repeats the line
// that Parse has already named.
func unwrapParseError(err error) error {
	var e *url.Error
	if errors.As(err, &e) {
		return e.Err
	}
	return err
}
