# Read-only live test against the toydaria test tenant, signing in with a
# federated identity credential (OIDC) from GitHub Actions. Run by
# .github/workflows/live-test.yml with the provider built from the commit.
#
# The identity holds read roles only, and this configuration has data sources
# only. A data source fails on any response other than 2xx, so a successful
# apply means every GET returned 2xx. The postconditions add that the response
# had a JSON body. Nothing from the responses is output, so the run log shows
# no tenant data.

terraform {
  required_providers {
    msrest = { source = "terraprovider/msrest" }
  }
}

# ARM_TENANT_ID and ARM_CLIENT_ID come from the workflow.
provider "msrest" {
  use_oidc = true
}

# First token audience: https://tasks.office.com, derived from the URL.
data "msrest_resource" "planner" {
  url     = "https://tasks.office.com/taskAPI/tenantAdminSettings/Settings"
  headers = { "OData-Version" = "4.0;NetFx" }

  response_export_values = { response = "@" }

  lifecycle {
    postcondition {
      condition     = self.output.response != null
      error_message = "The Planner settings request returned 2xx without a JSON body."
    }
  }
}

# Second token audience: the M365 License Manager application.
data "msrest_resource" "products" {
  url   = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products"
  scope = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"

  response_export_values = { response = "@" }

  lifecycle {
    postcondition {
      condition     = self.output.response != null
      error_message = "The self-service purchase request returned 2xx without a JSON body."
    }
  }
}
