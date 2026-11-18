package provider

import (
	"context"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/jmespath/go-jmespath"
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

// requestBody validates a request body given as an HCL value. It must be an
// object or a list. A string is rejected, so that a body written as
// jsonencode(...) out of habit is not sent as one JSON string.
func requestBody() validator.Dynamic { return requestBodyValidator{} }

type requestBodyValidator struct{}

func (requestBodyValidator) Description(context.Context) string {
	return "must be an HCL object or list, such as { key = value }"
}

func (v requestBodyValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v requestBodyValidator) ValidateDynamic(ctx context.Context, req validator.DynamicRequest, resp *validator.DynamicResponse) {
	c := req.ConfigValue
	if c.IsNull() || c.IsUnknown() || c.IsUnderlyingValueNull() || c.IsUnderlyingValueUnknown() {
		return
	}
	switch u := c.UnderlyingValue().(type) {
	case basetypes.ObjectValue, basetypes.MapValue, basetypes.ListValue, basetypes.TupleValue, basetypes.SetValue:
	case basetypes.StringValue:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid body",
			"The body is an HCL value, not a JSON string: write `"+req.Path.String()+" = { key = value }` "+
				"instead of `"+req.Path.String()+" = jsonencode({ key = value })`. "+
				"To use a JSON document from a file or variable, decode it: `"+req.Path.String()+" = jsondecode(...)`.")
	default:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid body",
			"The body "+v.Description(ctx)+", got a value of type "+u.Type(ctx).String()+".")
	}
}

// jmesPathQueries validates that every value of a map is a JMESPath expression.
func jmesPathQueries() validator.Map { return jmesPathQueriesValidator{} }

type jmesPathQueriesValidator struct{}

func (jmesPathQueriesValidator) Description(context.Context) string {
	return "values must be JMESPath expressions"
}

func (v jmesPathQueriesValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v jmesPathQueriesValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for name, e := range req.ConfigValue.Elements() {
		s, ok := e.(basetypes.StringValue)
		if !ok || s.IsNull() || s.IsUnknown() {
			continue
		}
		if _, err := jmespath.Compile(s.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path.AtMapKey(name), "Invalid JMESPath expression",
				"\""+s.ValueString()+"\": "+err.Error())
		}
	}
}
