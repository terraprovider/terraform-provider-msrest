# Read-only live test against the toydaria test tenant, signing in with a
# federated identity credential (OIDC) from GitHub Actions. Run by
# .github/workflows/live-test.yml with the provider built from the commit.
#
# The identity holds read roles only, and this configuration has data sources
# only. The checks test the shape of the values, not the values themselves,
# because the settings may be changed in the test tenant.

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

  response_export_values = { all = "@" }

  lifecycle {
    postcondition {
      condition     = can(tobool(self.output.all.allowCalendarSharing))
      error_message = "The Planner settings have no boolean allowCalendarSharing."
    }
  }
}

# Second token audience: the M365 License Manager application.
data "msrest_resource" "products" {
  url   = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products"
  scope = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"

  response_export_values = {
    items         = "items"
    policy_values = "items[].policyValue"
  }

  lifecycle {
    postcondition {
      condition     = length(self.output.items) > 0
      error_message = "The self-service purchase policy lists no products."
    }
    postcondition {
      condition = alltrue([
        for v in self.output.policy_values : contains(["Enabled", "Disabled", "OnlyTrialsWithoutPaymentMethod"], v)
      ])
      error_message = "A product has an unknown policyValue."
    }
  }
}

output "planner_allow_calendar_sharing" {
  value = data.msrest_resource.planner.output.all.allowCalendarSharing
}

output "self_service_purchase" {
  value = { for p in data.msrest_resource.products.output.items : p.productName => p.policyValue }
}
