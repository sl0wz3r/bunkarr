package plex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setProbeTimeouts shortens the probe's timeouts for one test.
func setProbeTimeouts(t *testing.T, each, total time.Duration) {
	t.Helper()
	oldEach, oldTotal := probeTimeout, probeTotalTimeout
	probeTimeout, probeTotalTimeout = each, total
	t.Cleanup(func() { probeTimeout, probeTotalTimeout = oldEach, oldTotal })
}

func TestProbeTimeouts(t *testing.T) {
	setProbeTimeouts(t, 100*time.Millisecond, 150*time.Millisecond)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); slow.Close() })
	cands := make([]Candidate, 12)
	for i := range cands {
		cands[i] = Candidate{URI: fmt.Sprintf("%s/p%d", slow.URL, i), Protocol: "http"}
	}
	start := time.Now()
	res := ProbeConnections(context.Background(), Options{}, "m", "tok-abcdefgh", cands)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the probe took %s; the total timeout is 150ms", d)
	}
	var timedOut, notTried int
	for _, r := range res {
		switch {
		case r.OK:
			t.Errorf("%+v", r)
		case strings.Contains(r.Message, "No answer within 100ms"), strings.Contains(r.Message, "No answer before the probe's 150ms limit"):
			timedOut++
		case strings.Contains(r.Message, "Not tried: the probe's 150ms limit"):
			notTried++
		default:
			t.Errorf("unexpected message %q", r.Message)
		}
	}
	// 4 at a time, 100 ms each, 150 ms in total: the first two rounds time out, the rest is never tried.
	if timedOut < 4 || notTried == 0 {
		t.Errorf("timed out %d, not tried %d", timedOut, notTried)
	}
}

func TestDescribeProbeError(t *testing.T) {
	dns := &Error{Method: "GET", Path: PathIdentity, Err: fmt.Errorf("dial tcp: %w", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true})}
	tests := []struct {
		err  error
		uri  string
		want string
	}{
		{dns, "https://192-168-1-2.abc.plex.direct:32400", "DNS-rebind protection"},
		{dns, "https://plex.example:32400", "could not be resolved"},
		{&Error{Method: "GET", Path: PathSections, StatusCode: 500, Err: errors.New("unexpected status 500")}, "http://x", "unexpected status (500)"},
		{context.Canceled, "http://x", "cancelled"},
		{errors.New("boom"), "http://x", "Could not connect: boom"},
	}
	for _, tt := range tests {
		if got := describeProbeError(tt.err, Candidate{URI: tt.uri}); !strings.Contains(got, tt.want) {
			t.Errorf("describeProbeError(%v, %s) = %q, want %q", tt.err, tt.uri, got, tt.want)
		}
	}
}
