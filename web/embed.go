// Package web holds the embedded admin UI and agent install script.
package web

import "embed"

//go:embed index.html app.js style.css install-agent.sh
var FS embed.FS
