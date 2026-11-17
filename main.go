// terraform-provider-msrest is a generic Terraform/OpenTofu provider for
// Microsoft REST APIs that are not covered by Microsoft Graph (for example the
// Planner tenant settings or the self-service purchase policies). It
// authenticates against Entra ID with the same configuration surface as the
// azuread/azurerm providers and requests a token for each API's own audience.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/terraprovider/terraform-provider-msrest/internal/provider"
)

// version is set by goreleaser at build time.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// Registry address; the same module publishes to the OpenTofu registry too.
		Address: "registry.terraform.io/terraprovider/msrest",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
