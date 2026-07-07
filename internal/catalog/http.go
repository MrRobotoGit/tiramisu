package catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// defaultMaxAttempts/defaultBaseDelay are the compiled-in fallback values, matching Do's
// historical hardcoded behavior (3 attempts, 1s/2s/4s exponential backoff) exactly.
const (
	defaultMaxAttempts = 3
	defaultBaseDelayMS = 1000
)

// retryMaxAttempts/retryBaseDelayMS hold a process-wide override set via SetRetryDefaults.
// Zero means "unset, use the compiled-in default" - so calling SetRetryDefaults(0, 0)
// resets to historical behavior (used by tests to restore global state between cases).
var (
	retryMaxAttempts atomic.Int64
	retryBaseDelayMS atomic.Int64
)

// SetRetryDefaults overrides the process-wide retry attempt count and base delay used by
// every Do call site that doesn't otherwise specify its own policy. Intended to be called
// once at startup (see main.go) from config.Config's MaxRetryAttempts/RetryDelayMS. Passing
// 0 (or a negative value) for either argument resets that value to the compiled-in default
// (3 attempts / 1000ms) rather than disabling retries or backoff — there is currently no way
// to configure "0 delay" or "0 attempts" through this API; callers wanting genuinely instant
// retries or to disable retrying entirely need a different mechanism.
//
// SetRetryDefaults is meant to be called once at startup, not concurrently with in-flight
// Do calls or from multiple goroutines racing each other: the two atomic stores (attempts,
// delay) aren't updated as a single atomic pair, so concurrent SetRetryDefaults calls could
// theoretically let one Do call observe a torn combination (old attempts + new delay). This
// is fine for the startup-only use case but should be revisited before repurposing this as a
// live-reload knob.
func SetRetryDefaults(maxAttempts int, baseDelay time.Duration) {
	retryMaxAttempts.Store(int64(maxAttempts))
	retryBaseDelayMS.Store(baseDelay.Milliseconds())
}

func currentMaxAttempts() int {
	if v := retryMaxAttempts.Load(); v > 0 {
		return int(v)
	}
	return defaultMaxAttempts
}

func currentBaseDelay() time.Duration {
	if v := retryBaseDelayMS.Load(); v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return defaultBaseDelayMS * time.Millisecond
}

// NewClient creates an HTTP client with the given timeout.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// Do executes an HTTP request with retry logic. Attempt count and base delay default to
// 3 attempts / 1s (doubling: 1s, 2s, 4s) unless overridden via SetRetryDefaults. Retries
// on network errors and 5xx responses; 4xx responses are returned as-is (not retried).
//
// Note: retry with a request body only works correctly when req.GetBody is set (auto-populated
// by http.NewRequest/NewRequestWithContext for *bytes.Buffer/*bytes.Reader/*strings.Reader bodies -
// true for every current caller of Do). A request built from a different io.Reader without GetBody
// set would silently resend an empty body on retry, same failure mode this fix addresses - callers
// with non-standard body types must set req.GetBody themselves or avoid retrying such requests.
func Do(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	attempts := currentMaxAttempts()
	base := currentBaseDelay()

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := base * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		attemptReq := req.Clone(ctx)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("failed to get request body for retry attempt %d: %w", attempt, err)
			}
			attemptReq.Body = body
		}

		resp, err := client.Do(attemptReq)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", attempts, lastErr)
}

// ReadAll reads and closes the response body.
func ReadAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
