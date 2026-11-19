package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/dynamicvalidator"
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
	defaultUpdateMethod          = http.MethodPatch
	defaultReadAfterWriteTimeout = 120 // seconds
)

// pollInterval is the delay between reads while waiting for a write to show up.
// A variable so tests can shorten it.
var pollInterval = 5 * time.Second

var (
	_ resource.Resource                = &updateResource{}
	_ resource.ResourceWithConfigure   = &updateResource{}
	_ resource.ResourceWithImportState = &updateResource{}
)

// NewUpdateResource returns the msrest_update_resource resource. Its name and
// attributes follow msgraph_update_resource of the msgraph provider.
func NewUpdateResource() resource.Resource { return &updateResource{} }

type updateResource struct{ client *Client }

type updateResourceModel struct {
	ID                    types.String  `tfsdk:"id"`
	URL                   types.String  `tfsdk:"url"`
	ReadURL               types.String  `tfsdk:"read_url"`
	Scope                 types.String  `tfsdk:"scope"`
	UpdateMethod          types.String  `tfsdk:"update_method"`
	Headers               types.Map     `tfsdk:"headers"`
	Body                  types.Dynamic `tfsdk:"body"`
	DeleteMethod          types.String  `tfsdk:"delete_method"`
	DeleteBody            types.Dynamic `tfsdk:"delete_body"`
	ReadAfterWriteTimeout types.Int64   `tfsdk:"read_after_write_timeout"`
	ResponseExportValues  types.Map     `tfsdk:"response_export_values"`
	Output                types.Dynamic `tfsdk:"output"`
	Concurrency           types.String  `tfsdk:"concurrency"`
}

func (r *updateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_update_resource"
}

func (r *updateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a subset of the properties of an existing object behind a REST API that is read with `GET` " +
			"and written with `PATCH`, `PUT` or `POST`, typically a settings object that always exists. " +
			"It works like `msgraph_update_resource` of the msgraph provider, for APIs other than Microsoft Graph.\n\n" +
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
			"update_method": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultUpdateMethod),
				MarkdownDescription: "HTTP method that writes `body` on create and update: `PATCH` (default), `PUT` or `POST`.",
				Validators:          []validator.String{stringvalidator.OneOf(http.MethodPatch, http.MethodPut, http.MethodPost)},
			},
			"headers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Additional request headers, sent with every request (for example `OData-Version`).",
			},
			"body": schema.DynamicAttribute{
				Required: true,
				MarkdownDescription: "The properties to set, as an HCL object such as `{ allowCalendarSharing = false }`. " +
					"It is sent as JSON. Only these properties are compared on refresh. " +
					"A property the API does not return is treated as write-only and never shows drift.",
				Validators: []validator.Dynamic{requestBody()},
			},
			"delete_method": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "HTTP method sent to `url` on destroy: `DELETE`, `PATCH`, `PUT` or `POST`. " +
					"When unset, destroying only removes the resource from the state and leaves the object as it is.",
				Validators: []validator.String{stringvalidator.OneOf(http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodPost)},
			},
			"delete_body": schema.DynamicAttribute{
				Optional: true,
				MarkdownDescription: "HCL object sent as JSON with `delete_method`, for example to restore defaults. " +
					"Without it, the destroy request has no body.",
				Validators: []validator.Dynamic{
					requestBody(),
					dynamicvalidator.AlsoRequires(path.MatchRoot("delete_method")),
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
			"response_export_values": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Values of the response to export to `output`: the key is the name in `output`, the value a " +
					"[JMESPath](https://jmespath.org/) query, `@` for the whole response. " +
					"For example `{ all = \"@\", sharing = \"allowCalendarSharing\" }`.",
				Validators: []validator.Map{jmesPathQueries()},
			},
			"concurrency": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of a concurrency group: resources in the same group write one at a time, " +
					"for APIs that reject parallel writes (the self-service purchase policies of the licensing API answer them " +
					"with HTTP 412). The group covers the write, the reads after it until the values show up " +
					"(`read_after_write_timeout`), and the request of `delete_method`. Reads on refresh are not serialized. " +
					"The group holds within one provider process, that is, one provider configuration of one run; " +
					"for writes from elsewhere, a 412 without a client precondition (`If-Match`, `If-None-Match`, " +
					"`If-Unmodified-Since`) is retried. Changing it writes nothing.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"output": schema.DynamicAttribute{
				Computed: true,
				MarkdownDescription: "The values selected by `response_export_values` from the last read, as an HCL object " +
					"(for example `msrest_update_resource.x.output.all`). An empty object when `response_export_values` is not set.",
				PlanModifiers: []planmodifier.Dynamic{outputWithoutExports{}},
			},
		},
	}
}

func (r *updateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *updateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m updateResourceModel
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

func (r *updateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, state updateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if onlyConcurrencyChanged(m, state) {
		// concurrency is not part of the object: store it without a write.
		m.ID, m.Output = state.ID, state.Output
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	r.write(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// write sends body, then reads the object back until it reflects body (or the
// timeout passes) and exports values of the last response. body itself stays as
// planned, so a slow API cannot make the apply inconsistent.
func (r *updateResource) write(ctx context.Context, m *updateResourceModel, diags *diag.Diagnostics) {
	desired, err := toJSON(ctx, m.Body)
	if err != nil {
		diags.AddAttributeError(path.Root("body"), "Invalid body", err.Error())
		return
	}
	payload, err := encodeJSON(desired)
	if err != nil {
		diags.AddAttributeError(path.Root("body"), "Encoding the body failed", err.Error())
		return
	}
	headers, d := stringMap(ctx, m.Headers)
	diags.Append(d...)
	queries, d := stringMap(ctx, m.ResponseExportValues)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	resource, err := tokenResource(m.Scope.ValueString(), m.URL.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("scope"), "Cannot determine the token scope", err.Error())
		return
	}

	release, err := writeGroups.acquire(ctx, m.Concurrency.ValueString())
	if err != nil {
		diags.AddError("Waiting for the concurrency group failed", err.Error())
		return
	}
	defer release()

	_, err = r.client.Do(ctx, Request{
		Method:   m.UpdateMethod.ValueString(),
		URL:      m.URL.ValueString(),
		Resource: resource,
		Headers:  headers,
		Body:     []byte(payload),
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
	// Without response_export_values the plan has output = {}, so set it even
	// when nothing could be read back.
	m.Output = types.DynamicNull()
	if len(queries) == 0 {
		m.Output = emptyOutput(ctx)
	}
	if seen {
		if missing := missingPaths(desired, last); len(missing) > 0 {
			diags.AddAttributeWarning(path.Root("body"), "Properties not returned by the API",
				fmt.Sprintf("%s does not return %s. These properties are treated as write-only and never show drift. "+
					"If that is not expected, check the property names: the API may have ignored them.",
					m.readURL(), strings.Join(missing, ", ")))
		}
		out, d := exportValues(ctx, last, queries)
		diags.Append(d...)
		m.Output = out
	}
}

func (r *updateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m updateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	headers, d := stringMap(ctx, m.Headers)
	resp.Diagnostics.Append(d...)
	queries, d := stringMap(ctx, m.ResponseExportValues)
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

	// body: keep the stored value while the API matches it, so that neither
	// number formatting nor the HCL types of the configuration (a tuple here, a
	// list there) cause a diff. Otherwise store the values the API returns.
	if m.Body.IsNull() || m.Body.IsUnderlyingValueNull() {
		// After an import there is no configured body yet: take the full response.
		b, err := fromJSONDynamic(ctx, actual)
		if err != nil {
			resp.Diagnostics.AddError("Converting the response failed", err.Error())
			return
		}
		m.Body = b
	} else {
		desired, err := toJSON(ctx, m.Body)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("body"), "Invalid body in state", err.Error())
			return
		}
		if projected := project(desired, actual); !jsonEqual(projected, desired) {
			b, err := fromJSONDynamic(ctx, projected)
			if err != nil {
				resp.Diagnostics.AddError("Converting the response failed", err.Error())
				return
			}
			m.Body = b
		}
	}

	out, d := exportValues(ctx, actual, queries)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.Output = out

	// Defaults for an imported object.
	m.ID = m.URL
	if m.UpdateMethod.IsNull() {
		m.UpdateMethod = types.StringValue(defaultUpdateMethod)
	}
	if m.ReadAfterWriteTimeout.IsNull() {
		m.ReadAfterWriteTimeout = types.Int64Value(defaultReadAfterWriteTimeout)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *updateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m updateResourceModel
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
	// No delete_body means a request without a body (not "null" or "{}").
	var body []byte
	if !m.DeleteBody.IsNull() && !m.DeleteBody.IsUnderlyingValueNull() {
		v, err := toJSON(ctx, m.DeleteBody)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("delete_body"), "Invalid delete_body", err.Error())
			return
		}
		s, err := encodeJSON(v)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("delete_body"), "Encoding delete_body failed", err.Error())
			return
		}
		body = []byte(s)
	}
	release, err := writeGroups.acquire(ctx, m.Concurrency.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Waiting for the concurrency group failed", err.Error())
		return
	}
	defer release()
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

// onlyConcurrencyChanged reports whether plan differs from state in nothing
// that is sent to the API, apart from concurrency. output is left out: it is
// unknown in the plan when response_export_values is set.
func onlyConcurrencyChanged(plan, state updateResourceModel) bool {
	return !plan.Concurrency.Equal(state.Concurrency) &&
		plan.URL.Equal(state.URL) &&
		plan.ReadURL.Equal(state.ReadURL) &&
		plan.Scope.Equal(state.Scope) &&
		plan.UpdateMethod.Equal(state.UpdateMethod) &&
		plan.Headers.Equal(state.Headers) &&
		plan.Body.Equal(state.Body) &&
		plan.DeleteMethod.Equal(state.DeleteMethod) &&
		plan.DeleteBody.Equal(state.DeleteBody) &&
		plan.ReadAfterWriteTimeout.Equal(state.ReadAfterWriteTimeout) &&
		plan.ResponseExportValues.Equal(state.ResponseExportValues)
}

// ImportState takes the object URL as the import ID.
func (r *updateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("url"), req.ID)...)
}

// get reads the object. found is false on HTTP 404.
func (r *updateResource) get(ctx context.Context, m *updateResourceModel, headers map[string]string) (any, bool, error) {
	return getJSON(ctx, r.client, m.readURL(), m.Scope.ValueString(), headers)
}

func (m *updateResourceModel) readURL() string {
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

// emptyOutput is output without response_export_values: an empty object.
func emptyOutput(ctx context.Context) types.Dynamic {
	v, _ := exportValues(ctx, nil, nil)
	return v
}

// outputWithoutExports plans output as an empty object when
// response_export_values is unset or empty, since that is all an apply can
// produce. Otherwise output stays unknown on create and update, because the
// exported values come from the response.
type outputWithoutExports struct{}

func (outputWithoutExports) Description(context.Context) string {
	return "An empty object when response_export_values is not set."
}

func (m outputWithoutExports) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (outputWithoutExports) PlanModifyDynamic(ctx context.Context, req planmodifier.DynamicRequest, resp *planmodifier.DynamicResponse) {
	if req.Plan.Raw.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}
	var queries types.Map
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("response_export_values"), &queries)...)
	if resp.Diagnostics.HasError() || queries.IsUnknown() || len(queries.Elements()) > 0 {
		return
	}
	resp.PlanValue = emptyOutput(ctx)
}
