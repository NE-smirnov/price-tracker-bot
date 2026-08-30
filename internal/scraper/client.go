package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultUserAgent identifies the bot honestly while still looking like a real
// browser build, which is what most shops key their markup on. Pretending to be
// nothing at all gets an immediate block from every host tested.
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 " +
	"price-tracker-bot/0.1 (+https://github.com/NE-smirnov/price-tracker-bot)"

// maxBodyBytes caps a downloaded page. Product pages run to a megabyte or two;
// the cap exists so one misbehaving host cannot exhaust the worker's memory.
const maxBodyBytes = 4 << 20

// ErrTemporary marks a failure that is worth retrying later: a timeout, a 5xx, a
// connection reset. It is separate from ErrNoPrice so the caller can tell "the
// shop is having a bad minute" from "this page no longer has a price".
var ErrTemporary = errors.New("temporary fetch failure")

// ErrNotFound means the page is gone. The item is most likely delisted, and no
// amount of retrying will bring the price back.
var ErrNotFound = errors.New("page not found")

// Client fetches product pages.
type Client struct {
	http      *http.Client
	userAgent string
	// perHostGap is the minimum delay between two requests to the same host. It
	// is what keeps a hundred tracked items on one shop from looking like an
	// attack, which is the fastest way to get an IP blocked permanently.
	perHostGap time.Duration
	maxRetries int

	mu       sync.Mutex
	nextFree map[string]time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// ClientOptions configures a Client.
type ClientOptions struct {
	Timeout    time.Duration
	UserAgent  string
	PerHostGap time.Duration
	MaxRetries int
	// Now and Sleep are injectable so the rate limiter can be tested without
	// spending real seconds.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// NewClient builds a fetcher.
func NewClient(opts ClientOptions) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}
	if opts.PerHostGap <= 0 {
		opts.PerHostGap = 2 * time.Second
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Compression is explicitly left to the transport: several shops serve a
		// usable page only when the client accepts gzip.
		ForceAttemptHTTP2: true,
	}

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   opts.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// A shop that bounces a request more than a few times is either
				// looping or funnelling the bot into a challenge page; both are
				// better reported than followed indefinitely.
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects (%d)", len(via))
				}
				return nil
			},
		},
		userAgent:  opts.UserAgent,
		perHostGap: opts.PerHostGap,
		maxRetries: opts.MaxRetries,
		nextFree:   map[string]time.Time{},
		now:        opts.Now,
		sleep:      opts.Sleep,
	}
}

// Page is a downloaded document.
type Page struct {
	URL  string
	Body []byte
	// ContentType is kept so a caller can tell an HTML page from a JSON API
	// response without sniffing the body twice.
	ContentType string
}

// Fetch downloads a page, respecting the per-host delay and retrying transient
// failures with exponential backoff.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return Page{}, fmt.Errorf("%w: %q is not a fetchable URL", ErrNoPrice, rawURL)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff, starting at the per-host gap: 2s, 4s, 8s.
			delay := c.perHostGap << (attempt - 1)
			if err := c.sleep(ctx, delay); err != nil {
				return Page{}, err
			}
		}
		if err := c.waitForHost(ctx, parsed.Host); err != nil {
			return Page{}, err
		}

		page, err := c.fetchOnce(ctx, rawURL)
		if err == nil {
			return page, nil
		}
		lastErr = err
		// Only transient failures are worth another attempt; a 404 or a block
		// will answer the same way however many times it is asked.
		if !errors.Is(err, ErrTemporary) {
			return Page{}, err
		}
	}
	return Page{}, lastErr
}

// waitForHost blocks until this host's next slot, and reserves the one after it.
func (c *Client) waitForHost(ctx context.Context, host string) error {
	c.mu.Lock()
	now := c.now()
	wait := time.Duration(0)
	if next, ok := c.nextFree[host]; ok && next.After(now) {
		wait = next.Sub(now)
	}
	// Reserve the slot before unlocking, so concurrent workers queue behind each
	// other instead of all deciding the host is free.
	c.nextFree[host] = now.Add(wait + c.perHostGap)
	c.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	return c.sleep(ctx, wait)
}

func (c *Client) fetchOnce(ctx context.Context, rawURL string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Page{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru,tr;q=0.9,en;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		// A cancelled context is the caller shutting down, not a shop problem.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Page{}, err
		}
		return Page{}, fmt.Errorf("%w: %w", ErrTemporary, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if statusErr := statusError(resp); statusErr != nil {
		return Page{}, statusErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Page{}, fmt.Errorf("%w: read body: %w", ErrTemporary, err)
	}

	return Page{
		URL:         resp.Request.URL.String(),
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// statusError maps an HTTP status onto the three outcomes the caller acts on.
func statusError(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil

	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w: %s", ErrNotFound, resp.Status)

	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limiting is transient by definition, and the shop usually says
		// how long to wait.
		return fmt.Errorf("%w: %s (retry-after %s)", ErrTemporary, resp.Status,
			retryAfter(resp.Header.Get("Retry-After")))

	case resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusUnauthorized ||
		// Cloudflare and Wildberries use these for bot protection.
		resp.StatusCode == 498 || resp.StatusCode == 499 ||
		resp.StatusCode == 503 && strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare"):
		return fmt.Errorf("%w: %s", ErrBlocked, resp.Status)

	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s", ErrTemporary, resp.Status)

	default:
		return fmt.Errorf("%w: unexpected status %s", ErrNoPrice, resp.Status)
	}
}

func retryAfter(header string) string {
	if header == "" {
		return "unset"
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		return (time.Duration(seconds) * time.Second).String()
	}
	return header
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
