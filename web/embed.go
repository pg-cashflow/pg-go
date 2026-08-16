package web

import "embed"

// Frozen operator shell. Production PWA is pg-react (FRONTEND_URL).
// Served at /app/ only when APP_ENV is not production (or FRONTEND_URL is empty).
// Do not add features here — update pg-react instead.

//go:embed index.html app.js styles.css manifest.json sw.js
var FS embed.FS
