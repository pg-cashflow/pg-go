package web

import "embed"

//go:embed index.html app.js styles.css manifest.json sw.js
var FS embed.FS
