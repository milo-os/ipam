package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubRoundTripper returns a canned response so the logging wrapper can be
// tested without a real server.
type stubRoundTripper struct {
	status string
	code   int
	err    error
}

func (s stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.code,
		Status:     s.status,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func TestLoggingRoundTripperLogsCallAndStatus(t *testing.T) {
	var buf bytes.Buffer
	rt := verboseTransport(&buf)(stubRoundTripper{status: "200 OK", code: 200})
	req, _ := http.NewRequest("GET", "https://api.example/apis/ipam.miloapis.com/v1alpha1/ippools", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"API call:", "GET", "/apis/ipam.miloapis.com/v1alpha1/ippools", "200 OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

func TestLoggingRoundTripperLogsError(t *testing.T) {
	var buf bytes.Buffer
	rt := verboseTransport(&buf)(stubRoundTripper{err: io.ErrUnexpectedEOF})
	req, _ := http.NewRequest("POST", "https://api.example/apis/ipam.miloapis.com/v1alpha1/namespaces/default/ipclaims", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected the underlying error to propagate")
	}
	if !strings.Contains(buf.String(), "error:") {
		t.Errorf("expected an error log line:\n%s", buf.String())
	}
}

func TestVlogfGatedByVerbose(t *testing.T) {
	// Off by default.
	ta := newTestApp(nil, &globalOptions{output: outputTable, color: "never"})
	ta.app.vlogf("should not appear")
	if ta.err.Len() != 0 {
		t.Fatalf("vlogf wrote without --verbose: %q", ta.err.String())
	}
	// On with --verbose.
	tv := newTestApp(nil, &globalOptions{output: outputTable, color: "never", verbose: true})
	tv.app.vlogf("resolved scope: %s", "acme / net-core")
	if !strings.Contains(tv.err.String(), "resolved scope: acme / net-core") {
		t.Fatalf("vlogf did not write under --verbose: %q", tv.err.String())
	}
}
