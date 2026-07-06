package main

// defaultDest maps each top-level Terraform block type to the file it
// belongs in. Block types not listed here are left where they are.
var defaultDest = map[string]string{
	"resource":  "main.tf",
	"module":    "main.tf",
	"data":      "data.tf",
	"locals":    "locals.tf",
	"variable":  "variables.tf",
	"output":    "outputs.tf",
	"provider":  "providers.tf",
	"terraform": "versions.tf",
	"moved":     "moved.tf",
	"import":    "imports.tf",
	"removed":   "removed.tf",
	"check":     "checks.tf",
	"ephemeral": "ephemeral.tf",
}
