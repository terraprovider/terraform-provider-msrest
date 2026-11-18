# Read-only check, for example with a plan identity that only holds
# Policy.Read.AllowSelfServicePurchase.
data "msrest_object" "self_service_purchase" {
  url   = "https://licensing.m365.microsoft.com/v1/policies/AllowSelfServicePurchase/products"
  scope = "aeb86249-8ea3-49e2-900b-54cc8e308f85/.default"
}

output "self_service_purchase" {
  value = {
    for p in jsondecode(data.msrest_object.self_service_purchase.response).items :
    p.productName => p.policyValue
  }
}
