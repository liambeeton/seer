package cli_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// redirectFixture serves /start, which 301s to /moved, which 302s to /final,
// which answers 200 with headers worth keeping: two Set-Cookie lines and a
// Server name, in an order no map would preserve.
func redirectFixture(t *testing.T) *httptest.Server {
	t.Helper()
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+"/moved", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+"/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "seer-fixture/1.0")
		w.Header().Add("Set-Cookie", "first=1; Path=/")
		w.Header().Add("Set-Cookie", "second=2; Path=/")
		w.Header().Set("X-Seer", "final hop")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Final</title></head><body>ok</body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	return srv
}

// headerValues finds the values recorded for one header name, in the order
// they were recorded.
func headerValues(hs []jsonlHeader, name string) []string {
	var out []string
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

func TestCapture_RedirectChainRecordsEveryHopIncludingTheLast(t *testing.T) {
	srv := redirectFixture(t)
	target := srv.URL + "/start"
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", target, "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 1 {
		t.Fatalf("got %d JSONL lines, want 1; stdout: %q", len(cs), r.stdout)
	}
	resp := cs[0].Response
	if resp == nil {
		t.Fatalf("response is null, want the redirected Response")
	}

	want := []jsonlHop{
		{srv.URL + "/start", 301},
		{srv.URL + "/moved", 302},
		{srv.URL + "/final", 200},
	}
	if len(resp.RedirectChain) != len(want) {
		t.Fatalf("redirect_chain = %+v, want %d hops", resp.RedirectChain, len(want))
	}
	for i, w := range want {
		if resp.RedirectChain[i] != w {
			t.Errorf("hop %d = %+v, want %+v", i, resp.RedirectChain[i], w)
		}
	}
	if resp.FinalURL != want[len(want)-1].URL {
		t.Errorf("final_url = %q, want the last hop's URL %q", resp.FinalURL, want[len(want)-1].URL)
	}
	if resp.Status != 200 {
		t.Errorf("response.status = %d, want the last hop's 200", resp.Status)
	}
	if resp.Title != "Final" {
		t.Errorf("response.title = %q, want %q", resp.Title, "Final")
	}
}

func TestCapture_HeadersOfTheFinalHopAreVerbatim(t *testing.T) {
	srv := redirectFixture(t)
	store := filepath.Join(t.TempDir(), "store")

	r := seer(t, append([]string{"capture", srv.URL + "/start", "-o", store, "--jsonl"}, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 1 || cs[0].Response == nil {
		t.Fatalf("want one Capture with a Response; stdout: %q", r.stdout)
	}
	hs := cs[0].Response.Headers

	// Both Set-Cookie lines survive as their own entries: a map would have
	// kept one, and Chrome's parsed headers drop them altogether.
	if got := headerValues(hs, "Set-Cookie"); len(got) != 2 || got[0] != "first=1; Path=/" || got[1] != "second=2; Path=/" {
		t.Errorf("Set-Cookie = %q, want both lines in order", got)
	}
	if got := headerValues(hs, "Server"); len(got) != 1 || got[0] != "seer-fixture/1.0" {
		t.Errorf("Server = %q, want [seer-fixture/1.0]", got)
	}
	if got := headerValues(hs, "X-Seer"); len(got) != 1 || got[0] != "final hop" {
		t.Errorf("X-Seer = %q, want [final hop]", got)
	}
	// The headers are the final hop's, not a redirect's.
	if got := headerValues(hs, "Location"); len(got) != 0 {
		t.Errorf("Location = %q, want none: that header belongs to a redirect hop", got)
	}
	// The server sent Server before Set-Cookie before X-Seer; wire order is
	// what "verbatim" means.
	if got := headerNames(hs); !inOrder(got, "Server", "Set-Cookie", "Set-Cookie", "X-Seer") {
		t.Errorf("header names = %q, want Server, Set-Cookie, Set-Cookie, X-Seer in that order", got)
	}
	if cs[0].Response.TLS != nil {
		t.Errorf("tls = %+v, want null for an http Target", *cs[0].Response.TLS)
	}
}

func headerNames(hs []jsonlHeader) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Name)
	}
	return out
}

// inOrder reports whether want appears in got as a subsequence.
func inOrder(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && strings.EqualFold(g, want[i]) {
			i++
		}
	}
	return i == len(want)
}

// certificate builds a self-signed certificate for 127.0.0.1, with mutate
// given the chance to spoil it.
func certificate(t *testing.T, mutate func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "seer fixture"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	mutate(template)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsFixture is a Target served over TLS with the given certificate.
func tlsFixture(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Secured</title></head><body>tls</body></html>`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestCapture_TLSSummaryRecordsCertificateAndTrustVerdict visits every broken
// certificate in one invocation: each Target is its own fixture server, and
// batching them pays for one browser launch instead of four.
func TestCapture_TLSSummaryRecordsCertificateAndTrustVerdict(t *testing.T) {
	selfSigned := tlsFixture(t, certificate(t, func(c *x509.Certificate) {}))
	expired := tlsFixture(t, certificate(t, func(c *x509.Certificate) {
		c.NotBefore = time.Now().Add(-48 * time.Hour)
		c.NotAfter = time.Now().Add(-24 * time.Hour)
	}))
	mismatched := tlsFixture(t, certificate(t, func(c *x509.Certificate) {
		c.Subject.CommonName = "elsewhere.invalid"
		c.DNSNames = []string{"elsewhere.invalid"}
		c.IPAddresses = nil
	}))
	plain := fixture200(t)
	store := filepath.Join(t.TempDir(), "store")

	args := []string{"capture", selfSigned.URL + "/", expired.URL + "/", mismatched.URL + "/", plain.URL + "/", "-o", store, "--jsonl"}
	r := seer(t, append(args, browserFlags()...)...)

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	cs := captures(t, r.stdout)
	if len(cs) != 4 {
		t.Fatalf("got %d JSONL lines, want 4; stdout: %q", len(cs), r.stdout)
	}

	// A certificate error never fails the visit: the page is still captured.
	for i, c := range cs[:3] {
		if c.Status != "succeeded" {
			t.Errorf("Capture %d of a broken certificate = %q, want succeeded (error %v)", i, c.Status, c.Error)
		}
		if c.ScreenshotPath == nil {
			t.Errorf("Capture %d has no Screenshot; a certificate error must not cost one", i)
		}
		if c.Response == nil || c.Response.Status != 200 {
			t.Errorf("Capture %d response = %+v, want a 200", i, c.Response)
		}
	}

	// The self-signed certificate: every field the summary promises.
	summary := cs[0].Response.TLS
	if summary == nil {
		t.Fatalf("tls is null for an https Target, want the certificate summary")
	}
	if summary.Subject != "seer fixture" {
		t.Errorf("tls.subject = %q, want %q", summary.Subject, "seer fixture")
	}
	if summary.Issuer != "seer fixture" {
		t.Errorf("tls.issuer = %q, want %q", summary.Issuer, "seer fixture")
	}
	if len(summary.SANs) != 1 || summary.SANs[0] != "127.0.0.1" {
		t.Errorf("tls.sans = %q, want [127.0.0.1]", summary.SANs)
	}
	if !strings.HasPrefix(summary.Protocol, "TLS") {
		t.Errorf("tls.protocol = %q, want a TLS version", summary.Protocol)
	}
	if summary.Cipher == "" {
		t.Errorf("tls.cipher is empty, want the negotiated cipher")
	}
	from := assertUTCTimestamp(t, "tls.valid_from", summary.ValidFrom)
	to := assertUTCTimestamp(t, "tls.valid_to", summary.ValidTo)
	if !to.After(from) {
		t.Errorf("tls.valid_to %s is not after tls.valid_from %s", summary.ValidTo, summary.ValidFrom)
	}
	if summary.Trusted {
		t.Errorf("tls.trusted = true, want false for a self-signed certificate")
	}
	if !strings.Contains(summary.Reason, "self-signed") {
		t.Errorf("tls.reason = %q, want it to name the certificate as self-signed", summary.Reason)
	}

	// Expired and name-mismatched certificates say so, even though each is
	// self-signed too.
	for _, tc := range []struct {
		name    string
		summary *jsonlTLS
		want    string
	}{
		{"expired", cs[1].Response.TLS, "expired"},
		{"name mismatch", cs[2].Response.TLS, "name mismatch"},
	} {
		if tc.summary == nil {
			t.Errorf("%s: tls is null, want the certificate summary", tc.name)
			continue
		}
		if tc.summary.Trusted {
			t.Errorf("%s: tls.trusted = true, want false", tc.name)
		}
		if !strings.Contains(tc.summary.Reason, tc.want) {
			t.Errorf("%s: tls.reason = %q, want it to mention %q", tc.name, tc.summary.Reason, tc.want)
		}
	}
	if validTo := assertUTCTimestamp(t, "expired tls.valid_to", cs[1].Response.TLS.ValidTo); !validTo.Before(time.Now()) {
		t.Errorf("expired: tls.valid_to = %q, want a time already past", cs[1].Response.TLS.ValidTo)
	}

	// An http Target has no certificate to summarise.
	if cs[3].Response == nil {
		t.Fatalf("the http Target has no Response")
	}
	if cs[3].Response.TLS != nil {
		t.Errorf("tls = %+v for an http Target, want null", *cs[3].Response.TLS)
	}
}
