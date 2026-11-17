// Package provider implements the msrest provider: a generic resource and data
// source for Microsoft REST APIs, authenticated against Entra ID.
package provider

import (
	"context"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/terraprovider/go-msadmin/auth"
	"github.com/terraprovider/tf-msadmin/authschema"
)

// accessTokenEnv supplies access_token when it is not set in the configuration.
const accessTokenEnv = "MSREST_ACCESS_TOKEN"

type msrestProvider struct{ version string }

// New returns the provider constructor for the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &msrestProvider{version: version} }
}

// providerModel embeds the shared azuread/azurerm-aligned auth block and adds
// a pre-acquired access token as an alternative.
type providerModel struct {
	authschema.Model
	AccessToken types.String `tfsdk:"access_token"`
}

func (p *msrestProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "msrest"
	resp.Version = p.version
}

func (p *msrestProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	attrs := authschema.Attributes()
	attrs["access_token"] = schema.StringAttribute{
		Optional:  true,
		Sensitive: true,
		Description: "A pre-acquired bearer token, sent as is with every request instead of authenticating " +
			"with the settings above. Its audience must match every API the provider instance calls, so use " +
			"one provider alias per audience. Env: " + accessTokenEnv + ".",
	}
	resp.Schema = schema.Schema{
		Description: "Generic provider for Microsoft REST APIs outside Microsoft Graph. It requests an Entra ID " +
			"token for each API's own audience. Authentication mirrors the AzureAD/AzureRM providers " +
			"(ARM_*/AZURE_* env vars supported).",
		Attributes: attrs,
	}
}

func (p *msrestProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var tokens auth.TokenProvider
	if t := firstNonEmpty(m.AccessToken.ValueString(), os.Getenv(accessTokenEnv)); t != "" {
		tokens = auth.StaticToken(t)
	} else {
		// Explicit config overlaid onto the ARM_*/AZURE_* environment. No token is
		// requested here: each request asks for its own audience.
		tp, err := m.Config().Build()
		if err != nil {
			resp.Diagnostics.AddError("Authentication configuration error", err.Error())
			return
		}
		tokens = tp
	}

	c := NewClient(tokens, "terraform-provider-msrest/"+p.version)
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *msrestProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource}
}

func (p *msrestProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
