package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/terraprovider/go-msadmin/httpx"
)

const (
	defaultMethod                = http.MethodPatch
	defaultReadAfterWriteTimeout = 120 // seconds
)

// pollInterval is the delay between reads while waiting for a write to show up.
// A variable so tests can shorten it.
var pollInterval = 5 * time.Second

var (
	_ resource.Resource                = &objectResource{}
	_ resource.ResourceWithConfigure   = &objectResource{}
	_ resource.ResourceWithImportState = &objectResource{}
)

// NewObjectResource returns the msrest_object resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct{ client *Client }

type objectModel struct {
	ID                    types.String `tfsdk:"id"`
	URL                   types.String `tfsdk:"url"`
	ReadURL               types.String `tfsdk:"read_url"`
	Scope                 types.String `tfsdk:"scope"`
	Method                types.String `tfsdk:"method"`
	Headers               types.Map    `tfsdk:"headers"`
	Body                  types.String `tfsdk:"body"`
	DeleteMethod          types.String `tfsdk:"delete_method"`
	DeleteBody            types.String `tfsdk:"delete_body"`
	ReadAfterWriteTimeout types.Int64  `tfsdk:"read_after_write_timeout"`
	Response              types.String `tfsdk:"response"`
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the properties of an object behind a REST API that is read with `GET` " +
			"and written with `PATCH`, `PUT` or `POST`, typically a settings object that always exists.\n\n" +
			"Only the properties in `body` are managed. On every refresh the provider reads the object and " +
			"compares just those properties, so a change made outside of Terraform shows up as a diff on `body`. " +
			"Destroying the resource only removes it from the state, unless `delete_method` is set.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The `url` of the object.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"url": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Absolute URL the object is written to (and read from, unless `read_url` is set). Changing it replaces the resource.",
				Validators:          []validator.String{absoluteURL()},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"read_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Absolute URL to read the object from, if it differs from `url`.",
				Validators:          []validator.String{absoluteURL()},
			},
			"scope": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "OAuth scope (or resource) of the token, for example `https://tasks.office.com/.default` " +
					"or `aeb86249-8ea3-49e2-900b-54cc8e308f85/.default`. Defaults to `https://<host of the request URL>/.default`.",
			},
			"method": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultMethod),
				MarkdownDescription: "HTTP method that writes `body` on create and update: `PATCH` (default), `PUT` or `POST`.",
				Validators:          []validator.String{stringvalidator.OneOf(http.MethodPatch, http.MethodPut, http.MethodPost)},
			},
			"headers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Additional request headers, sent with every request (for example `OData-Version`).",
			},
			"body": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON document with the properties to set, usually built with `jsonencode()`. " +
					"Only these properties are compared on refresh. A property the API does not return is treated as write-only and never shows drift.",
				Validators: []validator.String{validJSON()},
			},
			"delete_method": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "HTTP method sent to `url` on destroy: `DELETE`, `PATCH`, `PUT` or `POST`. " +
					"When unset, destroying only removes the resource from the state and leaves the object as it is.",
				Validators: []validator.String{stringvalidator.OneOf(http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodPost)},
			},
			"delete_body": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "JSON document sent with `delete_method`, for example to restore defaults.",
				Validators: []validator.String{
					validJSON(),
					stringvalidator.AlsoRequires(path.MatchRoot("delete_method")),
				},
			},
			"read_after_write_timeout": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultReadAfterWriteTimeout),
				MarkdownDescription: fmt.Sprintf("Seconds to keep reading after a write until the API returns the written values "+
					"(these APIs are eventually consistent). Defaults to `%d`; `0` reads once without waiting. "+
					"If the values do not show up in time, the apply still succeeds with a warning.", defaultReadAfterWriteTimeout),
				Validators: []validator.Int64{int64validator.AtLeast(0)},
			},
			"response": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The complete JSON response of the last read. Use `jsondecode()` to access its values.",
			},
		},
	}
}

func (r *objectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *Client, got %T", req.ProviderData))
		return
	}
	r.client = c
}

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// write sends body, then reads the object back until it reflects body (or the
// timeout passes) and stores the last response. body itself stays as planned,
// so a slow API cannot make the apply inconsistent.
func (r *objectResource) write(ctx context.Context, m *objectModel, diags *diag.Diagnostics) {
	desired, err := decodeJSON([]byte(m.Body.ValueString()))
	if err != nil {
		diags.AddAttributeError(path.Root("body"), "Invalid JSON", err.Error())
		return
	}
	headers, d := stringMap(ctx, m.Headers)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	resource, err := tokenResource(m.Scope.ValueString(), m.URL.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("scope"), "Cannot determine the token scope", err.Error())
		return
	}

	_, err = r.client.Do(ctx, Request{
		Method:   m.Method.ValueString(),
		URL:      m.URL.ValueString(),
		Resource: resource,
		Headers:  headers,
		Body:     []byte(m.Body.ValueString()),
	})
	if err != nil {
		diags.AddError("Writing the object failed", err.Error())
		return
	}
	m.ID = m.URL

	// Poll until the read reflects body or the timeout passes. A 404 (not
	// visible yet) or a 429 that outlasted the transport's retries means "try
	// again later"; any other error stops polling.
	timeout := m.ReadAfterWriteTimeout.ValueInt64()
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	var last any
	var seen, ok bool
	for {
		v, found, getErr := r.get(ctx, m, headers)
		err = getErr
		if err == nil && found {
			last, seen = v, true
			ok = reflects(desired, v)
		}
		if ok || (err != nil && !isThrottled(err)) || !time.Now().Add(pollInterval).Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(pollInterval):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if ok || (seen && isThrottled(err)) {
		err = nil
	}
	switch {
	case err != nil:
		diags.AddWarning("Reading the object after the write failed",
			"The write succeeded, but reading it back did not: "+err.Error())
	case !ok && timeout > 0:
		diags.AddWarning("The API does not return the written values yet",
			fmt.Sprintf("After %d seconds, %s still does not return all values of body. The write succeeded; "+
				"the next plan shows a diff on body until the change has propagated or if the API ignored a value.",
				timeout, m.readURL()))
	}
	m.Response = types.StringNull()
	if seen {
		if missing := missingPaths(desired, last); len(missing) > 0 {
			diags.AddAttributeWarning(path.Root("body"), "Properties not returned by the API",
				fmt.Sprintf("%s does not return %s. These properties are treated as write-only and never show drift. "+
					"If that is not expected, check the property names: the API may have ignored them.",
					m.readURL(), strings.Join(missing, ", ")))
		}
		s, err := encodeJSON(last)
		if err != nil {
			diags.AddError("Encoding the response failed", err.Error())
			return
		}
		m.Response = types.StringValue(s)
	}
}

func (r *objectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	headers, d := stringMap(ctx, m.Headers)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	actual, found, err := r.get(ctx, &m, headers)
	if err != nil {
		resp.Diagnostics.AddError("Reading the object failed", err.Error())
		return
	}
	if !found {
		tflog.Warn(ctx, "object not found, removing it from the state", map[string]any{"url": m.readURL()})
		resp.State.RemoveResource(ctx)
		return
	}

	// body: keep the stored text while the API matches it (so formatting never
	// causes a diff), otherwise store the values the API returns.
	if m.Body.IsNull() || m.Body.ValueString() == "" {
		// After an import there is no configured body yet: take the full response.
		s, err := encodeJSON(actual)
		if err != nil {
			resp.Diagnostics.AddError("Encoding the response failed", err.Error())
			return
		}
		m.Body = types.StringValue(s)
	} else {
		desired, err := decodeJSON([]byte(m.Body.ValueString()))
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("body"), "Invalid JSON in state", err.Error())
			return
		}
		if projected := project(desired, actual); !jsonEqual(projected, desired) {
			s, err := encodeJSON(projected)
			if err != nil {
				resp.Diagnostics.AddError("Encoding the response failed", err.Error())
				return
			}
			m.Body = types.StringValue(s)
		}
	}

	s, err := encodeJSON(actual)
	if err != nil {
		resp.Diagnostics.AddError("Encoding the response failed", err.Error())
		return
	}
	m.Response = types.StringValue(s)

	// Defaults for an imported object.
	m.ID = m.URL
	if m.Method.IsNull() {
		m.Method = types.StringValue(defaultMethod)
	}
	if m.ReadAfterWriteTimeout.IsNull() {
		m.ReadAfterWriteTimeout = types.Int64Value(defaultReadAfterWriteTimeout)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.DeleteMethod.IsNull() || m.DeleteMethod.ValueString() == "" {
		tflog.Info(ctx, "no delete_method set, leaving the object unchanged", map[string]any{"url": m.URL.ValueString()})
		return
	}
	headers, d := stringMap(ctx, m.Headers)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resource, err := tokenResource(m.Scope.ValueString(), m.URL.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("scope"), "Cannot determine the token scope", err.Error())
		return
	}
	var body []byte
	if !m.DeleteBody.IsNull() {
		body = []byte(m.DeleteBody.ValueString())
	}
	_, err = r.client.Do(ctx, Request{
		Method:   m.DeleteMethod.ValueString(),
		URL:      m.URL.ValueString(),
		Resource: resource,
		Headers:  headers,
		Body:     body,
	})
	if err != nil && !httpx.IsNotFound(err) {
		resp.Diagnostics.AddError("Deleting the object failed", err.Error())
	}
}

// ImportState takes the object URL as the import ID.
func (r *objectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("url"), req.ID)...)
}

// get reads the object. found is false on HTTP 404.
func (r *objectResource) get(ctx context.Context, m *objectModel, headers map[string]string) (any, bool, error) {
	return getJSON(ctx, r.client, m.readURL(), m.Scope.ValueString(), headers)
}

func (m *objectModel) readURL() string {
	if !m.ReadURL.IsNull() && m.ReadURL.ValueString() != "" {
		return m.ReadURL.ValueString()
	}
	return m.URL.ValueString()
}

// getJSON sends a GET and decodes the response. found is false on HTTP 404. An
// empty response body decodes to JSON null.
func getJSON(ctx context.Context, c *Client, rawURL, scope string, headers map[string]string) (any, bool, error) {
	resource, err := tokenResource(scope, rawURL)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, URL: rawURL, Resource: resource, Headers: headers})
	if httpx.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(resp.Body) == 0 {
		return nil, true, nil
	}
	v, err := decodeJSON(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("GET %s: the response is not JSON: %w", rawURL, err)
	}
	return v, true, nil
}

func stringMap(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}
	out := map[string]string{}
	d := m.ElementsAs(ctx, &out, false)
	return out, d
}

// isThrottled reports whether err is an HTTP 429 that outlasted the retries.
func isThrottled(err error) bool {
	var ae *httpx.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests
}
