// SPDX-License-Identifier: MIT

// Package web embeds the portal templates and static assets.
package web

import "embed"

//go:embed templates static
var FS embed.FS
