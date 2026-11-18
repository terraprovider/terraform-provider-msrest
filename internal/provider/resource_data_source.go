package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &resourceDataSource{}
	_ datasource.DataSourceWithConfigure = &resourceDataSource{}
)

// NewResourceDataSource returns the msrest_resource data source. Its name and
// attributes follow the msgraph_resource data source of the msgraph provider.
func NewResourceDataSource() datasource.DataSource { return &resourceDataSource{} }

type resourceDataSource struct{ client *Client }

type resourceDataSourceModel struct {
	ID                   types.String  `tfsdk:"id"`
	URL                  types.String  `tfsdk:"url"`
	Scope                types.String  `tfsdk:"scope"`
	Headers              types.Map     `tfsdk:"headers"`
	ResponseExportValues types.Map     `tfsdk:"response_export_values"`
	Output               types.Dynamic `tfsdk:"output"`
}

func (d *resourceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_resource"
}

func (d *resourceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a JSON object or collection with `GET`, for example to check settings or to look up IDs. " +
			"It works like the `msgraph_resource` data source of the msgraph provider, for APIs other than Microsoft Graph.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The `url`.",
			},
			"url": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Absolute URL to read.",
				Validators:          []validator.String{absoluteURL()},
			},
			"scope": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "OAuth scope (or resource) of the token, for example `https://tasks.office.com/.default`. " +
					"Defaults to `https://<host of url>/.default`.",
			},
			"headers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Additional request headers.",
			},
			"response_export_values": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Values of the response to export to `output`: the key is the name in `output`, the value a " +
					"[JMESPath](https://jmespath.org/) query, `@` for the whole response. " +
					"For example `{ all = \"@\", product_ids = \"items[].productId\" }`.",
				Validators: []validator.Map{jmesPathQueries()},
			},
			"output": schema.DynamicAttribute{
				Computed: true,
				MarkdownDescription: "The values selected by `response_export_values`, as an HCL object " +
					"(for example `data.msrest_resource.x.output.all`). An empty object when `response_export_values` is not set.",
			},
		},
	}
}

func (d *resourceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *Client, got %T", req.ProviderData))
		return
	}
	d.client = c
}

func (d *resourceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m resourceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	headers, diags := stringMap(ctx, m.Headers)
	resp.Diagnostics.Append(diags...)
	queries, diags := stringMap(ctx, m.ResponseExportValues)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	v, found, err := getJSON(ctx, d.client, m.URL.ValueString(), m.Scope.ValueString(), headers)
	if err != nil {
		resp.Diagnostics.AddError("Reading the object failed", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Object not found", fmt.Sprintf("GET %s returned 404", m.URL.ValueString()))
		return
	}
	out, diags := exportValues(ctx, v, queries)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = m.URL
	m.Output = out
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
