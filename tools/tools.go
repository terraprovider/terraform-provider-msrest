//go:build generate

package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)

// Format the Terraform example code used in the generated documentation.
//go:generate terraform fmt -recursive ../examples/

// Generate the registry documentation from the schemas and examples into ../docs.
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-dir .. --provider-name msrest
