// Package web embeds perchd's templates and static assets.
package web

import "embed"

//go:embed static templates
var FS embed.FS
