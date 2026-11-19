package provider

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/terraprovider/go-msadmin/httpx"
	"github.com/terraprovider/go-msadmin/retry"
)

func TestAPIErrorEnvelopes(t *testing.T) {
	r := Request{Method: "PUT", URL: "https://example.com/x"}
	cases := []struct {
		body, code, message string
	}{
		{`{"error":{"code":"BadRequest","message":"Invalid value"}}`, "BadRequest", "PUT https://example.com/x: Invalid value"},
		{`{"errorCode":"ProductNotSupported","reason":"The policy is not applicable to the product."}`, "ProductNotSupported", "PUT https://example.com/x: The policy is not applicable to the product."},
		{`{"code":"Forbidden","message":"Missing role"}`, "Forbidden", "PUT https://example.com/x: Missing role"},
		{`not json`, "", "PUT https://example.com/x: Bad Request: not json"},
	}
	for _, c := range cases {
		var ae *httpx.APIError
		if !errors.As(apiError(r, 400, []byte(c.body)), &ae) {
			t.Fatalf("not an APIError for %s", c.body)
		}
		if ae.Status != 400 || ae.Code != c.code || ae.Message != c.message {
			t.Errorf("apiError(%s) = %d %q %q, want 400 %q %q", c.body, ae.Status, ae.Code, ae.Message, c.code, c.message)
		}
	}
}

// cosmosConflict is the licensing API's answer to colliding writes (#14).
const cosmosConflict = `{"errorCode":"PreconditionFailed","reason":"Response status code does not indicate success: PreconditionFailed (412); Substatus: 0; Reason: (Errors : [\"One of the specified pre-condition is not met.\"]);"}`

// conflictServer answers the first `conflicts` requests with 412 and the rest
// with 200, and records the bodies it received.
type conflictServer struct {
	mu        sync.Mutex
	conflicts int
	bodies    []string
}

func (s *conflictServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, string(data))
	if len(s.bodies) <= s.conflicts {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, cosmosConflict)
		return
	}
	_, _ = io.WriteString(w, `{"policyValue":"Enabled"}`)
}

func testConflictClient(maxAttempts int) *http.Client {
	return &http.Client{Transport: &preconditionTransport{
		Base:   http.DefaultTransport,
		Config: retry.Config{MaxAttempts: maxAttempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	}}
}

func putJSON(t *testing.T, c *http.Client, url, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPreconditionRetry(t *testing.T) {
	s := &conflictServer{conflicts: 3}
	srv := httptest.NewServer(s)
	defer srv.Close()

	const body = `{"policyValue":"Enabled"}`
	resp := putJSON(t, testConflictClient(5), srv.URL, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(s.bodies) != 4 {
		t.Fatalf("%d requests, want 4 (3 x 412, then 200)", len(s.bodies))
	}
	for i, b := range s.bodies {
		if b != body {
			t.Errorf("request %d sent body %q, want %q", i+1, b, body)
		}
	}
}

func TestPreconditionRetryGivesUp(t *testing.T) {
	s := &conflictServer{conflicts: 100}
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp := putJSON(t, testConflictClient(4), srv.URL, `{}`, nil)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != cosmosConflict {
		t.Errorf("body = %q, want the last 412 body", b)
	}
	if len(s.bodies) != 4 {
		t.Errorf("%d requests, want 4 (MaxAttempts)", len(s.bodies))
	}
}

// With a client precondition, 412 is the answer to it and is not retried.
func TestPreconditionRetryNotWithClientPrecondition(t *testing.T) {
	for _, h := range preconditionHeaders {
		t.Run(h, func(t *testing.T) {
			s := &conflictServer{conflicts: 1}
			srv := httptest.NewServer(s)
			defer srv.Close()

			resp := putJSON(t, testConflictClient(5), srv.URL, `{}`, map[string]string{h: `"etag"`})
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Errorf("status = %d, want 412", resp.StatusCode)
			}
			if len(s.bodies) != 1 {
				t.Errorf("%d requests, want 1", len(s.bodies))
			}
		})
	}
}

func TestFullJitter(t *testing.T) {
	cfg := retry.Config{BaseDelay: time.Second, MaxDelay: 30 * time.Second}
	for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 40: 30 * time.Second} {
		seen := map[time.Duration]bool{}
		for range 200 {
			d := fullJitter(cfg, attempt)
			if d < 0 || d > ceiling {
				t.Fatalf("fullJitter(attempt %d) = %v, want within [0, %v]", attempt, d, ceiling)
			}
			seen[d] = true
		}
		// Colliding requests must not wait the same time.
		if len(seen) < 100 {
			t.Errorf("fullJitter(attempt %d): only %d distinct waits in 200 draws", attempt, len(seen))
		}
	}
}
