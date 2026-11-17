package provider

import (
	"errors"
	"testing"

	"github.com/terraprovider/go-msadmin/httpx"
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
