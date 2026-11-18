# terraform-provider-msrest

Generic Terraform/OpenTofu provider for Microsoft REST APIs that Microsoft Graph does not cover, such as the Planner tenant settings (`tasks.office.com`) or the self-service purchase policies (`licensing.m365.microsoft.com`).

It authenticates against Entra ID and requests a token for each API's own audience. Authentication is configured the same way as in the other TerraProvider providers ([exo](https://github.com/terraprovider/terraform-provider-exo), teams, spo): the attributes and `ARM_*`/`AZURE_*` environment variables match the `azuread`/`azurerm` providers. The auth code comes from [tf-msadmin](https://github.com/terraprovider/tf-msadmin) and [go-msadmin](https://github.com/terraprovider/go-msadmin).

## Authentication

| Method | Settings |
| --- | --- |
| Client secret | `tenant_id`, `client_id`, `client_secret` (or `ARM_TENANT_ID`, `ARM_CLIENT_ID`, `ARM_CLIENT_SECRET`) |
| Federated identity credential (OIDC) | `use_oidc = true` plus the pipeline's token source: GitHub Actions (`ACTIONS_ID_TOKEN_REQUEST_URL`/`_TOKEN`), Azure DevOps (`ado_pipeline_service_connection_id`), a token (`oidc_token`) or a token file (`oidc_token_file_path`, `AZURE_FEDERATED_TOKEN_FILE`) |
| Client certificate | `client_certificate` or `client_certificate_path`, `client_certificate_password` |
| Azure CLI | `use_cli = true` |
| Managed identity | `use_msi = true` |
| Pre-acquired token | `access_token` (or `MSREST_ACCESS_TOKEN`). It is sent as is, so use one provider alias per token audience. |

## `msrest_update_resource` resource

Manages a subset of the properties of an object that already exists, reading it with `GET` and writing it with `PATCH`, `PUT` or `POST`. It is meant for settings objects that always exist. It works like [`msgraph_update_resource`](https://registry.terraform.io/providers/microsoft/msgraph/latest/docs/resources/update_resource) of the msgraph provider, with the same attribute names, for APIs other than Microsoft Graph.

```hcl
resource "msrest_update_resource" "planner" {
  url     = "https://tasks.office.com/taskAPI/tenantAdminSettings/Settings"
  headers = { "OData-Version" = "4.0;NetFx" }
  body    = { allowCalendarSharing = false }
}

resource "msrest_update_resource" "copilot_self_service_purchase" {
  url           = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products/CFQ7TTC0MM8R"
  scope         = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
  update_method = "PUT"
  body          = { policyValue = "Disabled" }
}
```

| Attribute | |
| --- | --- |
| `url` | Required. Where `body` is written, and read from unless `read_url` is set. Changing it replaces the resource. |
| `body` | Required. The properties to set, as an HCL object; it is sent as JSON. Not a string: write `body = { ... }`, not `body = jsonencode({ ... })`. For a JSON document from a file, use `jsondecode()`. |
| `update_method` | `PATCH` (default), `PUT` or `POST`. |
| `scope` | Token scope, for example `https://tasks.office.com/.default` or `<application id>/.default`. Defaults to `https://<host of the URL>/.default`, which only works if that host is an App ID URI of the API (true for Planner, not for licensing). |
| `headers` | Extra request headers. |
| `read_url` | Read from a different URL. |
| `delete_method`, `delete_body` | Request sent on destroy, for example to restore a default. Without `delete_body` the request has no body. Without `delete_method`, destroy only removes the resource from the state. |
| `read_after_write_timeout` | Seconds to keep reading after a write until the API returns the written values (default `120`, `0` = read once). |
| `response_export_values` | Values of the response to put into `output`: name = [JMESPath](https://jmespath.org/) query, `@` for the whole response, for example `{ all = "@", name = "productName" }`. |
| `output` | Computed. The values selected by `response_export_values` from the last read, as an HCL object (`msrest_update_resource.x.output.name`). An empty object when `response_export_values` is not set. |

How it behaves:

- **Only the properties in `body` are managed.** Each refresh reads the object and compares just those properties, by value: key order and number formatting are ignored, and keys match case-insensitively. A change made outside of Terraform shows up as a diff on `body`, and the next apply writes the value again. Other properties of the object are ignored.
- **A property the API does not return** is treated as write-only and never shows drift. After a write, the provider warns about such properties, so a misspelled name gets noticed.
- **Eventual consistency.** After a write, the provider keeps reading until the API returns the written values, so the next plan does not show stale drift. Planner, for example, takes about 30 s. If the values do not show up within `read_after_write_timeout`, the apply still succeeds with a warning.
- **Import.** `tofu import msrest_update_resource.planner https://tasks.office.com/taskAPI/tenantAdminSettings/Settings`. The first plan after the import writes `body` once.
- Throttling (429, honouring `Retry-After`) and transient 5xx errors are retried with backoff (up to 8 attempts). The Planner Tasks API throttles after a few reads within seconds.
- A 404 on read removes the resource from the state.
- Numbers are sent as written: integers without exponent (`2000000`, not `2e+06`).

## `msrest_resource` data source

Reads a URL with `GET` and exports values of the response to `output`, like the [`msgraph_resource`](https://registry.terraform.io/providers/microsoft/msgraph/latest/docs/data-sources/resource) data source. Use it to check settings with a read-only identity or to look up IDs.

```hcl
data "msrest_resource" "planner" {
  url = "https://tasks.office.com/taskAPI/tenantAdminSettings/Settings"

  response_export_values = { sharing = "allowCalendarSharing" }
}

output "planner_calendar_sharing" {
  value = data.msrest_resource.planner.output.sharing
}
```

Attributes: `url` (required), `scope`, `headers`, `response_export_values` and `output`, as for the resource. A 404 is an error.

## Known APIs

| Setting | URL | Method | Scope | App role (write / read) |
| --- | --- | --- | --- | --- |
| Planner tenant settings | `https://tasks.office.com/taskAPI/tenantAdminSettings/Settings` | `PATCH` | `https://tasks.office.com/.default` (default) | `OrgSettings-Planner.ReadWrite.All` / `OrgSettings-Planner.Read.All` on ProjectWorkManagement (`09abbdfd-ed23-44ee-a2d9-a627aa1c90f3`) |
| Self-service purchase, per product | `https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products/{productId}` | `PUT` | `aeb86249-8ea3-49e2-900b-54cc8e308f85/.default` | `Policy.ReadWrite.AllowSelfServicePurchase` / `Policy.Read.AllowSelfServicePurchase` on M365 License Manager (`aeb86249-8ea3-49e2-900b-54cc8e308f85`) |

`policyValue` is `Enabled`, `Disabled` or `OnlyTrialsWithoutPaymentMethod`. `GET .../products` lists all products of the tenant; the catalog differs per tenant. A product ID the tenant does not have returns HTTP 400 with `errorCode` `ProductNotSupported` (not 404).

## Development

```bash
go build ./...
go vet ./...
gofmt -l .
# Unit and mock-API tests. They need terraform or tofu; with OpenTofu:
TF_ACC_TERRAFORM_PATH=$(which tofu) TF_ACC_PROVIDER_HOST=registry.opentofu.org go test -race ./...
```

To try a local build, point a CLI config file at the build directory:

```hcl
# dev.tofurc, used with TF_CLI_CONFIG_FILE=dev.tofurc
provider_installation {
  dev_overrides {
    "registry.opentofu.org/terraprovider/msrest" = "/path/to/terraform-provider-msrest/bin"
  }
  direct {}
}
```

### Releases

Pushing a `v*` tag starts the Release workflow (`.github/workflows/release.yml`). It builds the provider with GoReleaser (version pinned in `aqua/aqua.yml`), signs the checksums with the GPG key from the `release` environment (secrets `GPG_PRIVATE_KEY`, `PASSPHRASE`) and creates a draft release. Check its assets, then publish it. The Build workflow runs the same build as a snapshot, without signing, on every push.

## License

MIT — see [LICENSE](LICENSE). Not affiliated with or endorsed by Microsoft.
