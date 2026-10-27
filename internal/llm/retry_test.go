package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodoc/internal/trace"
)

func TestRetryTransport(t *testing.T) {
	var bodies []string
	statuses := []int{503, 429, 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(statuses[len(bodies)-1])
	}))
	defer srv.Close()

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rt := newRetryTransport(rec, "m")
	var waits []time.Duration
	rt.sleep = func(d time.Duration) { waits = append(waits, d) }

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("payload"))
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(bodies) != 3 || bodies[0] != "payload" || bodies[2] != "payload" {
		t.Errorf("bodies = %q, want the payload replayed 3 times", bodies)
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 4*time.Second {
		t.Errorf("waits = %v, want [2s 4s]", waits)
	}
	if got := rec.Totals().Retries; got != 2 {
		t.Errorf("retries in trace = %d, want 2", got)
	}
}

func TestRetryTransportGivesUp(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rt := newRetryTransport(rec, "m")
	rt.sleep = func(time.Duration) {}

	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || calls != rt.attempts {
		t.Errorf("status = %d after %d calls, want 503 after %d", resp.StatusCode, calls, rt.attempts)
	}
}

func TestRetryTransportTimesOut(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			<-r.Context().Done() // stall until the client gives up
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rt := newRetryTransport(rec, "m")
	rt.timeout = 100 * time.Millisecond
	rt.sleep = func(time.Duration) {}

	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body) // must still be readable: the deadline is cancelled on Close, not on return
	resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Errorf("body = %q, %v; want ok", body, err)
	}
	if calls != 2 || rec.Totals().Retries != 1 {
		t.Errorf("calls = %d, retries = %d; want 2 and 1", calls, rec.Totals().Retries)
	}
}

func TestBackoff(t *testing.T) {
	rt := &retryTransport{base: 2 * time.Second, max: 30 * time.Second}
	for _, tt := range []struct {
		attempt    int
		retryAfter string
		want       time.Duration
	}{
		{1, "", 2 * time.Second},
		{4, "", 16 * time.Second},
		{6, "", 30 * time.Second},
		{1, "10", 10 * time.Second},
		{1, "120", 30 * time.Second},
		{3, "1", 8 * time.Second},
	} {
		if got := rt.backoff(tt.attempt, tt.retryAfter); got != tt.want {
			t.Errorf("backoff(%d, %q) = %s, want %s", tt.attempt, tt.retryAfter, got, tt.want)
		}
	}
}
