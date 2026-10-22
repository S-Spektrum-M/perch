// Package web embeds pircd's templates and static assets.
package web

import "embed"

//go:embed static templates
var FS embed.FS
