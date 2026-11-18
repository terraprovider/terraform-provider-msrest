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
	_ datasource.DataSource              = &objectDataSource{}
	_ datasource.DataSourceWithConfigure = &objectDataSource{}
)

// NewObjectDataSource returns the msrest_object data source.
func NewObjectDataSource() datasource.DataSource { return &objectDataSource{} }

type objectDataSource struct{ client *Client }

type objectDataSourceModel struct {
	ID       types.String `tfsdk:"id"`
	URL      types.String `tfsdk:"url"`
	Scope    types.String `tfsdk:"scope"`
	Headers  types.Map    `tfsdk:"headers"`
	Response types.String `tfsdk:"response"`
}

func (d *objectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (d *objectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a JSON object with `GET`, for example to check settings or to look up IDs.",
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
			"response": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The JSON response. Use `jsondecode()` to access its values.",
			},
		},
	}
}

func (d *objectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *objectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m objectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	headers, diags := stringMap(ctx, m.Headers)
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
	s, err := encodeJSON(v)
	if err != nil {
		resp.Diagnostics.AddError("Encoding the response failed", err.Error())
		return
	}
	m.ID = m.URL
	m.Response = types.StringValue(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
