package openapi

import _ "embed"

//go:embed openapi.yaml
var Spec []byte

//go:embed docs.html
var DocsHTML []byte
