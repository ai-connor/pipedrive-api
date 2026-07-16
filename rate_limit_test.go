package pipedriveapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastConfig returns a config with tiny delays so tests run quickly.
func fastConfig() RateLimitConfig {
	c := DefaultRateLimitConfig()
	c.BaseDelay = time.Millisecond
	c.MaxDelay = 5 * time.Millisecond
	return c
}

func newTestClient(rlc RateLimitConfig) *http.Client {
	return &http.Client{Transport: &rateLimitTransport{cfg: rlc.sanitized()}}
}

func TestRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := newTestClient(fastConfig())
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 calls (2 retries), got %d", got)
	}
}

func TestExhaustsRetriesAndReturns429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rlc := fastConfig()
	rlc.MaxRetries = 2
	client := newTestClient(rlc)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after exhausting retries, got %d", resp.StatusCode)
	}
	// initial attempt + 2 retries == 3 calls
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
}

func TestNonRetryablePassesThrough(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestClient(fastConfig())
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 call for non-retryable status, got %d", got)
	}
}

func TestRespectsRetryAfterHeader(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rlc := DefaultRateLimitConfig()
	rlc.BaseDelay = time.Millisecond // would be ~0-1ms without Retry-After
	rlc.MaxDelay = 5 * time.Second
	client := newTestClient(rlc)

	start := time.Now()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("expected to honour Retry-After (>=1s), waited only %v", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := newTestClient(DefaultRateLimitConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fast return on context cancel, took %v", elapsed)
	}
}

func TestBodyIsReplayedOnRetry(t *testing.T) {
	var bodies []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(fastConfig())
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`{"name":"acme"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, b := range bodies {
		if b != `{"name":"acme"}` {
			t.Fatalf("attempt %d received wrong body: %q", i, b)
		}
	}
}

func TestOnRetryCallbackInvoked(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var retries int32
	rlc := fastConfig()
	rlc.OnRetry = func(attempt int, delay time.Duration, resp *http.Response) {
		atomic.AddInt32(&retries, 1)
	}
	client := newTestClient(rlc)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if got := atomic.LoadInt32(&retries); got != 1 {
		t.Fatalf("expected OnRetry to fire once, fired %d", got)
	}
}

func TestEnableRateLimitingInstallsTransport(t *testing.T) {
	cfg := NewConfiguration()
	cfg.EnableRateLimiting()

	if cfg.HTTPClient == nil {
		t.Fatal("expected HTTPClient to be set")
	}
	if _, ok := cfg.HTTPClient.Transport.(*rateLimitTransport); !ok {
		t.Fatalf("expected transport to be *rateLimitTransport, got %T", cfg.HTTPClient.Transport)
	}
	// Must not mutate the global default client.
	if http.DefaultClient.Transport != nil {
		t.Fatal("http.DefaultClient.Transport was mutated")
	}
}

func TestEnableRateLimitingPreservesBaseTransport(t *testing.T) {
	base := &http.Transport{}
	cfg := NewConfiguration()
	cfg.HTTPClient = &http.Client{Transport: base}
	cfg.EnableRateLimiting()

	rt, ok := cfg.HTTPClient.Transport.(*rateLimitTransport)
	if !ok {
		t.Fatalf("expected *rateLimitTransport, got %T", cfg.HTTPClient.Transport)
	}
	if rt.base != base {
		t.Fatal("expected existing base transport to be preserved")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("5"); !ok || d != 5*time.Second {
		t.Fatalf("seconds parse failed: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("empty value should not parse")
	}
	if _, ok := parseRetryAfter("-3"); ok {
		t.Fatal("negative seconds should not parse")
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Fatalf("http-date parse failed: %v %v", d, ok)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(past); !ok || d != 0 {
		t.Fatalf("past http-date should clamp to 0: %v %v", d, ok)
	}
}

func TestSanitizedFillsDefaults(t *testing.T) {
	c := RateLimitConfig{}.sanitized()
	d := DefaultRateLimitConfig()
	if c.BaseDelay != d.BaseDelay || c.MaxDelay != d.MaxDelay {
		t.Fatalf("zero delays not defaulted: %+v", c)
	}
	if len(c.RetryableStatusCodes) == 0 {
		t.Fatal("retryable status codes not defaulted")
	}

	// MaxDelay below BaseDelay should be raised to BaseDelay.
	c2 := RateLimitConfig{BaseDelay: 10 * time.Second, MaxDelay: time.Second}.sanitized()
	if c2.MaxDelay < c2.BaseDelay {
		t.Fatalf("MaxDelay should be >= BaseDelay, got %v < %v", c2.MaxDelay, c2.BaseDelay)
	}
}
