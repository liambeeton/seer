package capture

import (
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Response is what the Target's server returned during the visit.
type Response struct {
	StatusCode int
	FinalURL   string
	Title      string
	// RedirectChain is every hop the main frame took, in order, the final
	// one included; a Target that does not redirect has a chain of one.
	RedirectChain []Hop
	// Headers are the final hop's response headers as the server sent them:
	// in wire order, duplicates kept.
	Headers []Header
	// TLS summarises the certificate; nil when the final hop was not served
	// over TLS.
	TLS *TLS
}

// Hop is one step of a redirect chain: the URL that was requested and the
// status it answered with.
type Hop struct {
	URL    string
	Status int
}

// Header is one response header, exactly as it came over the wire.
type Header struct {
	Name  string
	Value string
}

// TLS is the certificate the Target served and the browser's verdict on it.
type TLS struct {
	Subject   string
	SANs      []string
	Issuer    string
	ValidFrom time.Time
	ValidTo   time.Time
	Protocol  string
	Cipher    string
	// Trusted is whether the browser accepted the certificate.
	Trusted bool
	// Reason is why it did not; empty when it did.
	Reason string
}

// documentWatcher assembles the Response from the main frame's document
// request: every redirect hop, the final hop's headers and the certificate the
// browser saw.
type documentWatcher struct {
	mu sync.Mutex
	// requestID is the main frame's document request. CDP keeps one id for a
	// whole redirect chain, which is also what tells the chain's events apart
	// from every subresource's.
	requestID proto.NetworkRequestID
	// hops are the chain's redirects; the final hop comes from last.
	hops []Hop
	last *proto.NetworkResponse
	// raw is the latest raw-headers event for requestID. One arrives per hop,
	// so the last to arrive is the final hop's.
	raw *proto.NetworkResponseReceivedExtraInfo
	// cert is the latest certificate the page's security state reported.
	cert *proto.SecurityCertificateSecurityState
	stop func()
}

func watchDocument(page *rod.Page) *documentWatcher {
	w := &documentWatcher{}
	events, cancel := page.WithCancel()
	wait := events.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			if e.Type != proto.NetworkResourceTypeDocument || e.FrameID != page.FrameID {
				return
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if e.RedirectResponse != nil && e.RequestID == w.requestID {
				w.hops = append(w.hops, Hop{URL: e.RedirectResponse.URL, Status: e.RedirectResponse.Status})
				return
			}
			// A document request with no redirect behind it is a navigation of
			// its own. The Response describes the document finally shown, so
			// its chain starts here.
			w.requestID, w.hops, w.last, w.raw = e.RequestID, nil, nil, nil
		},
		func(e *proto.NetworkResponseReceived) {
			if e.Type != proto.NetworkResourceTypeDocument || e.FrameID != page.FrameID {
				return
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			w.last = e.Response
		},
		func(e *proto.NetworkResponseReceivedExtraInfo) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if e.RequestID == w.requestID {
				w.raw = e
			}
		},
		func(e *proto.SecurityVisibleSecurityStateChanged) {
			w.mu.Lock()
			defer w.mu.Unlock()
			// A page whose security state is about something other than its
			// certificate (mixed content, say) reports no certificate at all;
			// the last one seen stays the Target's.
			if s := e.VisibleSecurityState; s != nil && s.CertificateSecurityState != nil {
				w.cert = s.CertificateSecurityState
			}
		},
	)
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

// response is what has been observed so far, or nil while nothing has come
// back from the server at all.
func (w *documentWatcher) response() *Response {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		return nil
	}
	final := Hop{URL: w.last.URL, Status: w.last.Status}
	return &Response{
		StatusCode:    final.Status,
		FinalURL:      final.URL,
		RedirectChain: append(slices.Clone(w.hops), final),
		Headers:       headers(w.raw, w.last),
		TLS:           summarise(w.last, w.cert),
	}
}

// headers is the final hop's response headers. Chrome's raw-headers event
// carries the header block as text over HTTP/1.x, which is the only source
// that keeps wire order and duplicates; over HTTP/2 and QUIC there is no such
// text, leaving the header maps, which join duplicate values with newlines and
// have no order of their own.
func headers(raw *proto.NetworkResponseReceivedExtraInfo, last *proto.NetworkResponse) []Header {
	if raw != nil {
		if raw.HeadersText != "" {
			return parseHeaderBlock(raw.HeadersText)
		}
		if pairs := flatten(raw.Headers); pairs != nil {
			return pairs
		}
	}
	// The parsed headers are a last resort: Chrome strips Set-Cookie from them.
	return flatten(last.Headers)
}

// parseHeaderBlock splits a raw HTTP/1.x response header block into pairs,
// keeping the server's order and its repeated names.
func parseHeaderBlock(text string) []Header {
	var pairs []Header
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "HTTP/") {
			continue // the status line, which is not a header
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue // the blank line that ends the block
		}
		pairs = append(pairs, Header{Name: name, Value: strings.TrimSpace(value)})
	}
	return pairs
}

// flatten turns a CDP header map into pairs, splitting the newline-joined
// values Chrome uses for a repeated name. Map order is random, so the names are
// sorted: an arbitrary order that at least does not change between Captures.
func flatten(h proto.NetworkHeaders) []Header {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	var pairs []Header
	for _, name := range names {
		for value := range strings.SplitSeq(h[name].Str(), "\n") {
			pairs = append(pairs, Header{Name: name, Value: value})
		}
	}
	return pairs
}

// summarise describes the certificate the final hop was served with, or nil
// when it was not served over TLS at all.
func summarise(last *proto.NetworkResponse, cert *proto.SecurityCertificateSecurityState) *TLS {
	details := last.SecurityDetails
	if details == nil {
		return nil
	}
	t := &TLS{
		Subject:   details.SubjectName,
		SANs:      details.SanList,
		Issuer:    details.Issuer,
		ValidFrom: details.ValidFrom.Time().UTC().Truncate(time.Millisecond),
		ValidTo:   details.ValidTo.Time().UTC().Truncate(time.Millisecond),
		Protocol:  details.Protocol,
		Cipher:    details.Cipher,
	}

	// The page's security state is the browser's verdict. Without one, the
	// response's own state still separates a clean handshake from a rejected
	// certificate.
	netErr := ""
	if cert != nil {
		netErr = cert.CertificateNetworkError
	} else if last.SecurityState != proto.SecuritySecurityStateSecure {
		netErr = "certificate error"
	}
	t.recordVerdict(netErr, hostOf(last.URL))
	return t
}

// recordVerdict records whether the browser trusted the certificate and what is
// wrong with it when it did not. The browser's word is the verdict, but it
// names only the highest-priority fault it found, and an untrusted issuer
// outranks the rest: a self-signed certificate that is also long expired and
// issued for another name arrives described as none of those. So the validity
// window and the names the certificate carries are read from the certificate
// too, and what the browser said is added to them.
func (t *TLS) recordVerdict(netErr, host string) {
	t.Trusted = netErr == ""
	if t.Trusted {
		return
	}
	var faults []string
	switch now := now(); {
	case !t.ValidTo.IsZero() && t.ValidTo.Before(now):
		faults = append(faults, "expired")
	case !t.ValidFrom.IsZero() && t.ValidFrom.After(now):
		faults = append(faults, "not yet valid")
	}
	if host != "" && !t.covers(host) {
		faults = append(faults, "name mismatch")
	}
	if fault := t.browserFault(netErr); fault != "" && !slices.Contains(faults, fault) {
		faults = append(faults, fault)
	}
	t.Reason = strings.Join(faults, ", ")
}

// browserFault puts the browser's certificate error into the words the Report
// reads in. An untrusted issuer that signed the certificate itself is a
// self-signed certificate, which is what the operator wants to be told; an
// internal CA is not, and saying so is the difference between a lab host and a
// suspicious one. An error seer has no words for is passed on as the browser
// wrote it.
func (t *TLS) browserFault(netErr string) string {
	switch netErr {
	case "net::ERR_CERT_AUTHORITY_INVALID":
		if t.Subject != "" && t.Subject == t.Issuer {
			return "self-signed"
		}
		return "untrusted issuer"
	case "net::ERR_CERT_DATE_INVALID":
		return "expired"
	case "net::ERR_CERT_COMMON_NAME_INVALID", "net::ERR_CERT_NAME_CONSTRAINT_VIOLATION":
		return "name mismatch"
	case "net::ERR_CERT_REVOKED":
		return "revoked"
	case "net::ERR_CERT_INVALID":
		return "malformed"
	case "net::ERR_CERT_WEAK_SIGNATURE_ALGORITHM":
		return "weak signature"
	}
	return netErr
}

// covers reports whether one of the certificate's subject alternative names
// stands for host, a leading-label wildcard included.
func (t *TLS) covers(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, san := range t.SANs {
		san = strings.ToLower(strings.TrimSuffix(san, "."))
		if san == host {
			return true
		}
		if suffix, wild := strings.CutPrefix(san, "*."); wild {
			if _, parent, ok := strings.Cut(host, "."); ok && parent == suffix {
				return true
			}
		}
	}
	return false
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
