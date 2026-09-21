// Package openapi embeds the OpenAPI 3.0 specification and the interactive
// viewer served by the API at /openapi.yaml and /docs.
package openapi

import _ "embed"

// Spec is the canonical OpenAPI specification, served at /openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte

// DocsHTML is the Swagger UI viewer, served at /docs.
//
//go:embed docs.html
var DocsHTML []byte
