terraform {
  required_providers {
    msrest = {
      source = "terraprovider/msrest"
    }
  }
}

# Same settings and ARM_* environment variables as the azuread/azurerm providers.
# In GitHub Actions with a federated credential:
provider "msrest" {
  use_oidc = true
}

# Locally with a client secret (or set ARM_TENANT_ID / ARM_CLIENT_ID / ARM_CLIENT_SECRET):
# provider "msrest" {
#   tenant_id     = "00000000-0000-0000-0000-000000000000"
#   client_id     = "00000000-0000-0000-0000-000000000000"
#   client_secret = var.client_secret
# }

# With a token fetched elsewhere (one alias per token audience):
# provider "msrest" {
#   alias        = "planner"
#   access_token = var.planner_token
# }
