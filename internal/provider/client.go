package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/terraprovider/go-msadmin/auth"
	"github.com/terraprovider/go-msadmin/httpx"
	"github.com/terraprovider/go-msadmin/retry"
)

// maxErrorBody bounds the response body kept in an error message.
const maxErrorBody = 4096

// Client sends authenticated JSON requests. It is shared by every resource and
// data source through Configure.
type Client struct {
	Tokens    auth.TokenProvider
	HTTP      *http.Client
	UserAgent string
}

// retryConfig bounds the retries of throttled (429) and transient (5xx)
// responses. The admin APIs throttle aggressively (the Planner Tasks API
// answers 429 after a few reads within seconds), so it is more patient than the
// go-msadmin default. A variable so tests can shorten it.
var retryConfig = retry.Config{MaxAttempts: 8, BaseDelay: 2 * time.Second, MaxDelay: 60 * time.Second}

// NewClient returns a Client that retries throttling and transient errors and
// logs the full traffic (credentials redacted) at TF_LOG=DEBUG.
func NewClient(tokens auth.TokenProvider, userAgent string) *Client {
	return &Client{
		Tokens: tokens,
		HTTP: &http.Client{
			Transport: retry.NewTransport(httpx.NewDebugTransport(nil), retryConfig),
			Timeout:   10 * time.Minute,
		},
		UserAgent: userAgent,
	}
}

// Request is one API call.
type Request struct {
	Method string
	URL    string
	// Resource is the token audience (an App ID URI such as
	// https://tasks.office.com, or an application ID).
	Resource string
	Headers  map[string]string
	Body     []byte // nil sends no body
}

// Response is a successful (2xx) API response.
type Response struct {
	Status int
	Body   []byte
}

// Do sends r and returns the response. A non-2xx status is returned as an
// *httpx.APIError, so callers can use httpx.IsNotFound.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	token, err := c.Tokens.Token(ctx, r.Resource)
	if err != nil {
		err = fmt.Errorf("acquiring a token for %q: %w", r.Resource, err)
		if strings.Contains(err.Error(), "AADSTS500011") {
			// The audience derived from the URL host is not an App ID URI of the API.
			err = fmt.Errorf("%w\n\nSet `scope` to the API's own audience, usually \"<application id>/.default\"", err)
		}
		return nil, err
	}

	var body io.Reader
	if r.Body != nil {
		// bytes.Reader lets net/http set GetBody, so the retry transport can replay it.
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("client-request-id", httpx.NewCorrelationID())
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", r.Method, r.URL, err)
	}
	data, err := httpx.DecodeBody(resp)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the response: %w", r.Method, r.URL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(r, resp.StatusCode, data)
	}
	return &Response{Status: resp.StatusCode, Body: data}, nil
}

// apiError builds an *httpx.APIError from a failed response, reading the usual
// Microsoft error envelopes: {"error":{"code","message"}} (Graph, OData),
// {"code","message"} and {"errorCode","reason"} (licensing API).
func apiError(r Request, status int, data []byte) error {
	e := &httpx.APIError{Status: status, Body: truncate(string(data), maxErrorBody)}

	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		ErrorCode string `json:"errorCode"`
		Reason    string `json:"reason"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		switch {
		case envelope.Error != nil:
			e.Code, e.Message = envelope.Error.Code, envelope.Error.Message
		case envelope.ErrorCode != "" || envelope.Reason != "":
			e.Code, e.Message = envelope.ErrorCode, envelope.Reason
		default:
			e.Code, e.Message = envelope.Code, envelope.Message
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
		if b := strings.TrimSpace(e.Body); b != "" {
			e.Message += ": " + b
		}
	}
	e.Message = fmt.Sprintf("%s %s: %s", r.Method, r.URL, e.Message)
	return e
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tokenResource returns the token audience for a request to rawURL. scope may
// be empty (the audience is then https://<host of rawURL>), a v2 scope such as
// "https://tasks.office.com/.default", or a bare resource such as an
// application ID.
func tokenResource(scope, rawURL string) (string, error) {
	if s := strings.TrimSpace(scope); s != "" {
		return strings.TrimSuffix(s, "/.default"), nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("cannot derive a token scope from %q: not an absolute URL", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}
