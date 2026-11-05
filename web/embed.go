package web

import "embed"

// Files contains the self-contained web application.
//
//go:embed index.html app.js style.css
var Files embed.FS
