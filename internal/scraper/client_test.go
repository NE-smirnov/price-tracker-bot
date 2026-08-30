package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock records the delays a client asked for instead of spending them, so
// the rate limiter and the backoff can be asserted on without slow tests.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

func newTestClient(clock *fakeClock, gap time.Duration, retries int) *Client {
	return NewClient(ClientOptions{
		PerHostGap: gap,
		MaxRetries: retries,
		Now:        clock.Now,
		Sleep:      clock.Sleep,
	})
}

func TestFetchSpacesRequestsToTheSameHost(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer server.Close()

	clock := newFakeClock()
	client := newTestClient(clock, 2*time.Second, 0)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := client.Fetch(ctx, server.URL+fmt.Sprintf("/p/%d", i)); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}

	if hits.Load() != 3 {
		t.Fatalf("server hits = %d, want 3", hits.Load())
	}
	// The first request goes out immediately; each later one waits out the gap.
	// Without this, a hundred items on one shop would arrive as a burst, which is
	// the fastest way to get the address blocked.
	want := []time.Duration{2 * time.Second, 2 * time.Second}
	got := clock.delays()
	if len(got) != len(want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want %v", got, want)
		}
	}
}

func TestFetchRateLimitIsPerHost(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html></html>"))
	})
	first := httptest.NewServer(handler)
	defer first.Close()
	second := httptest.NewServer(handler)
	defer second.Close()

	clock := newFakeClock()
	client := newTestClient(clock, 2*time.Second, 0)
	ctx := context.Background()

	if _, err := client.Fetch(ctx, first.URL); err != nil {
		t.Fatalf("first host: %v", err)
	}
	if _, err := client.Fetch(ctx, second.URL); err != nil {
		t.Fatalf("second host: %v", err)
	}
	// Different shops are unrelated, so one must not slow down the other.
	if delays := clock.delays(); len(delays) != 0 {
		t.Fatalf("delays = %v, want none across different hosts", delays)
	}
}

func TestFetchRetriesTransientFailures(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer server.Close()

	clock := newFakeClock()
	client := newTestClient(clock, 2*time.Second, 3)

	page, err := client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(string(page.Body), "ok") {
		t.Fatalf("body = %q, want the successful response", page.Body)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}

	// Backoff doubles: 2s before the second attempt, 4s before the third. The
	// per-host gap is charged on top of each.
	delays := clock.delays()
	if len(delays) < 2 || delays[0] != 2*time.Second {
		t.Fatalf("delays = %v, want the backoff to start at 2s", delays)
	}
	var sawFour bool
	for _, d := range delays {
		if d == 4*time.Second {
			sawFour = true
		}
	}
	if !sawFour {
		t.Fatalf("delays = %v, want a 4s backoff on the third attempt", delays)
	}
}

func TestFetchClassifiesStatusCodes(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		want    error
		// retried says whether the client should have tried more than once.
		retried bool
	}{
		{name: "not found", status: http.StatusNotFound, want: ErrNotFound},
		{name: "gone", status: http.StatusGone, want: ErrNotFound},
		{name: "forbidden is a block", status: http.StatusForbidden, want: ErrBlocked},
		{name: "wildberries 498 is a block", status: 498, want: ErrBlocked},
		{
			name:    "cloudflare 503 is a block",
			status:  http.StatusServiceUnavailable,
			headers: map[string]string{"Server": "cloudflare"},
			want:    ErrBlocked,
		},
		{name: "plain 503 is transient", status: http.StatusServiceUnavailable, want: ErrTemporary, retried: true},
		{name: "too many requests is transient", status: http.StatusTooManyRequests, want: ErrTemporary, retried: true},
		{name: "teapot is not a price", status: http.StatusTeapot, want: ErrNoPrice},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			clock := newFakeClock()
			client := newTestClient(clock, time.Second, 2)

			_, err := client.Fetch(context.Background(), server.URL)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// Retrying a block or a 404 wastes the host's patience and ours.
			if tc.retried && attempts.Load() == 1 {
				t.Fatalf("attempts = 1, want the client to retry a transient failure")
			}
			if !tc.retried && attempts.Load() != 1 {
				t.Fatalf("attempts = %d, want exactly 1 for a permanent outcome", attempts.Load())
			}
		})
	}
}

func TestFetchCapsTheBodySize(t *testing.T) {
	// A host that streams endlessly must not be able to exhaust the worker.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 64*1024)
		for i := 0; i < 128; i++ { // 8 MiB, twice the cap
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := newTestClient(newFakeClock(), time.Second, 0)
	page, err := client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(page.Body) != maxBodyBytes {
		t.Fatalf("body = %d bytes, want the %d-byte cap", len(page.Body), maxBodyBytes)
	}
}

func TestFetchStopsOnRedirectLoops(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/next", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestClient(newFakeClock(), time.Second, 0)
	if _, err := client.Fetch(context.Background(), server.URL); err == nil {
		t.Fatal("expected an error for an endless redirect chain")
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestClient(newFakeClock(), time.Second, 0)
	if _, err := client.Fetch(ctx, server.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
