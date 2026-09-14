// Package localshunt holds the prompts embedded into the local-shunt binary.
package localshunt

import "embed"

// Prompts are the system prompts for the worker model.
//
//go:embed prompts/*.md
var Prompts embed.FS
