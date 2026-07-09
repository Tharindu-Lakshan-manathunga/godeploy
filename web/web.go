// Package web embeds the dashboard's static assets directly into the
// godeploy binary, so deploying the controller itself is "copy one file,
// run it" — no separate frontend build or asset directory to manage.
package web

import "embed"

//go:embed static
var Static embed.FS
