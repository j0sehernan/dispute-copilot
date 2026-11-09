package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// HTTPDoer posts JSON to a provider and retries rate limits and server
// errors a couple of times before giving up.
type HTTPDoer struct {
	Client   *http.Client
	Provider string
	Headers  map[string]string
	Attempts int
	// Backoff is the first wait between attempts; it doubles each time.
	Backoff time.Duration
}

func (d *HTTPDoer) PostJSON(ctx context.Context, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", d.Provider, err)
	}

	attempts := max(d.Attempts, 1)
	wait := d.Backoff
	if wait == 0 {
		wait = time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryAfter, err := d.post(ctx, url, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrTemporary) || attempt == attempts {
			break
		}

		sleep := wait + time.Duration(rand.Int64N(int64(wait/2)+1))
		if retryAfter > 0 {
			sleep = retryAfter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
		wait *= 2
	}
	return lastErr
}

func (d *HTTPDoer) post(ctx context.Context, url string, body []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range d.Headers {
		req.Header.Set(k, v)
	}

	resp, err := d.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// Connection resets and client timeouts are worth another try.
		return 0, fmt.Errorf("%s: %w: %w", d.Provider, ErrTemporary, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, fmt.Errorf("%s: read response: %w: %w", d.Provider, ErrTemporary, err)
	}
	if resp.StatusCode/100 != 2 {
		return retryAfter(resp.Header.Get("Retry-After")), &StatusError{
			Provider: d.Provider,
			Code:     resp.StatusCode,
			Body:     truncate(string(data), 300),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return 0, fmt.Errorf("%s: decode response: %w", d.Provider, err)
	}
	return 0, nil
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 && secs <= 60 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
