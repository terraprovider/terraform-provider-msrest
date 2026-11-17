package provider

import (
	"context"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// absoluteURL validates an http(s) URL with a host.
func absoluteURL() validator.String { return absoluteURLValidator{} }

type absoluteURLValidator struct{}

func (absoluteURLValidator) Description(context.Context) string {
	return "must be an absolute http(s) URL"
}

func (v absoluteURLValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v absoluteURLValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	u, err := url.Parse(req.ConfigValue.ValueString())
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid URL",
			"\""+req.ConfigValue.ValueString()+"\" "+v.Description(ctx)+".")
	}
}

// validJSON validates that a string holds one JSON value.
func validJSON() validator.String { return validJSONValidator{} }

type validJSONValidator struct{}

func (validJSONValidator) Description(context.Context) string { return "must be valid JSON" }

func (v validJSONValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v validJSONValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := decodeJSON([]byte(req.ConfigValue.ValueString())); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid JSON", err.Error())
	}
}
