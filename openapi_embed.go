package transcoder

import _ "embed"

// OpenAPISchema contains the HTTP server's OpenAPI 3.1 description.
//
//go:embed openapi.yaml
var OpenAPISchema []byte
