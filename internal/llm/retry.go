package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	sleep    func(time.Duration) // replaced in tests
}

func newRetryTransport(rec *trace.Recorder, model string) *retryTransport {
	return &retryTransport{
		next:     http.DefaultTransport,
		rec:      rec,
		model:    model,
		attempts: 3,
		timeout:  90 * time.Second,
		base:     2 * time.Second,
		max:      30 * time.Second,
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
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
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

// backoff doubles from base, capped at max; a Retry-After in seconds wins if it is longer.
func (t *retryTransport) backoff(attempt int, retryAfter string) time.Duration {
	d := min(t.base<<(attempt-1), t.max)
	if s, err := strconv.Atoi(retryAfter); err == nil && time.Duration(s)*time.Second > d {
		d = min(time.Duration(s)*time.Second, t.max)
	}
	return d
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
