package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"autodoc/internal/trace"
)

// retryTransport retries model API requests that failed with a transient
// status (rate limit, overload) and records every retry in the trace. It does
// this at the HTTP layer so a retry stays inside one ADK model call.
type retryTransport struct {
	next     http.RoundTripper
	rec      *trace.Recorder
	model    string
	attempts int
	timeout  time.Duration // per attempt
	base     time.Duration
	max      time.Duration
	maxHint  time.Duration       // cap on a wait the server asks for; at least max
	sleep    func(time.Duration) // replaced in tests
}

const defaultTimeout = 90 * time.Second

// newRetryTransport builds the transport; a zero timeout means defaultTimeout.
func newRetryTransport(rec *trace.Recorder, model string, timeout time.Duration) *retryTransport {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &retryTransport{
		next:     http.DefaultTransport,
		rec:      rec,
		model:    model,
		attempts: 3,
		timeout:  timeout,
		base:     2 * time.Second,
		max:      30 * time.Second,
		// The free tier's per-minute quota answers 429 with "retry in ~40s".
		maxHint: 60 * time.Second,
	}
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		if attempt > 1 && req.Body != nil {
			if req.GetBody == nil {
				return nil, fmt.Errorf("cannot retry %s: request body is not replayable", req.URL.Path)
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		// Each attempt gets its own deadline so a stalled connection cannot hang the run.
		attemptCtx, cancel := context.WithTimeout(req.Context(), t.timeout)
		resp, err := t.next.RoundTrip(req.WithContext(attemptCtx))
		last := attempt == t.attempts

		var reason, retryAfter string
		switch {
		case err != nil:
			cancel()
			timedOut := errors.Is(err, context.DeadlineExceeded) && req.Context().Err() == nil
			if !timedOut || last {
				return nil, err
			}
			reason = fmt.Sprintf("model API did not respond within %s", t.timeout)
		case !retryable(resp.StatusCode) || last:
			// The deadline must outlive this call: the caller still reads the body.
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		default:
			retryAfter = resp.Header.Get("Retry-After")
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if retryAfter == "" {
				retryAfter = retryDelay(body)
			}
			if t.hintTooLong(retryAfter) {
				// A quota that resets in hours, not seconds: hand the error back
				// now so the next model in the chain takes over.
				resp.Body = &cancelOnClose{ReadCloser: io.NopCloser(bytes.NewReader(body)), cancel: cancel}
				return resp, nil
			}
			cancel()
			reason = fmt.Sprintf("model API returned %d", resp.StatusCode)
		}

		wait := t.backoff(attempt, retryAfter)
		t.rec.Emit(trace.Event{
			Type:   trace.Retry,
			Model:  t.model,
			Detail: fmt.Sprintf("%s; attempt %d of %d, retrying in %s", reason, attempt, t.attempts, wait),
		})
		if err := t.wait(req, wait); err != nil {
			return nil, err
		}
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// backoff doubles from base, capped at max; a Retry-After in seconds wins if
// it is longer, capped at maxHint.
func (t *retryTransport) backoff(attempt int, retryAfter string) time.Duration {
	d := min(t.base<<(attempt-1), t.max)
	if s, err := strconv.Atoi(retryAfter); err == nil && time.Duration(s)*time.Second > d {
		d = min(time.Duration(s)*time.Second, max(t.max, t.maxHint))
	}
	return d
}

// hintTooLong reports whether the server asks for a longer wait than maxHint.
func (t *retryTransport) hintTooLong(retryAfter string) bool {
	s, err := strconv.Atoi(retryAfter)
	return err == nil && time.Duration(s)*time.Second > max(t.max, t.maxHint)
}

var retryDelayRE = regexp.MustCompile(`"retryDelay":\s*"([0-9.]+)s"`)

// retryDelay reads the google.rpc.RetryInfo delay that Gemini puts in a 429
// body instead of a Retry-After header, as whole seconds rounded up.
func retryDelay(body []byte) string {
	m := retryDelayRE.FindSubmatch(body)
	if m == nil {
		return ""
	}
	f, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return ""
	}
	return strconv.Itoa(int(math.Ceil(f)))
}

func (t *retryTransport) wait(req *http.Request, d time.Duration) error {
	if t.sleep != nil {
		t.sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-req.Context().Done():
		return req.Context().Err()
	}
}
