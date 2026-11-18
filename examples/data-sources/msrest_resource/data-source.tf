# Read-only check, for example with a plan identity that only holds
# Policy.Read.AllowSelfServicePurchase.
data "msrest_resource" "self_service_purchase" {
  url   = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products"
  scope = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"

  response_export_values = { items = "items" }
}

output "self_service_purchase" {
  value = {
    for p in data.msrest_resource.self_service_purchase.output.items :
    p.productName => p.policyValue
  }
}
