# Planner tenant settings (singleton). Needs the app role
# OrgSettings-Planner.ReadWrite.All on ProjectWorkManagement. The token scope
# defaults to https://tasks.office.com/.default (the host of url).
resource "msrest_object" "planner" {
  url     = "https://tasks.office.com/taskAPI/tenantAdminSettings/Settings"
  headers = { "OData-Version" = "4.0;NetFx" }
  body = jsonencode({
    allowCalendarSharing = false
  })
}

# Self-service purchase policy, one object per product. Needs the app role
# Policy.ReadWrite.AllowSelfServicePurchase on M365 License Manager, whose token
# audience is its application ID, so the scope must be set.
locals {
  self_service_purchase = {
    CFQ7TTC0MM8R = "Disabled" # Microsoft 365 Copilot
    CFQ7TTC0LH3L = "Disabled" # Power Automate per user
  }
}

resource "msrest_object" "self_service_purchase" {
  for_each = local.self_service_purchase

  url    = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products/${each.key}"
  scope  = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
  method = "PUT"
  body   = jsonencode({ policyValue = each.value })

  # Optional: restore the default when the resource is destroyed.
  delete_method = "PUT"
  delete_body   = jsonencode({ policyValue = "Enabled" })
}

output "copilot_product_name" {
  value = jsondecode(msrest_object.self_service_purchase["CFQ7TTC0MM8R"].response).productName
}
