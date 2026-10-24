package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// dist/ holds the built pg-react assets.
// A committed dist/index.html guarantees clean checkouts compile and
// pass `go test ./...` even before the frontend has ever been built.
//
//go:embed dist/*
var distFS embed.FS

// hashedAssetRE matches Vite-style content-hashed filenames under assets/,
// e.g. assets/main-BHJd8sAA.js, assets/query-BJas__-u.js, assets/OtpInput-0Lmij3-_.js
var hashedAssetRE = regexp.MustCompile(`^assets/.+-[A-Za-z0-9_-]{8}\.[a-z0-9]+$`)

// Handler serves embedded static files and falls back to index.html for
// unmatched client-side routes.
func Handler() gin.HandlerFunc {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return func(c *gin.Context) {
		reqPath := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/")
		if reqPath == "" || reqPath == "." {
			reqPath = "index.html"
		}

		// Explicit landing page handling: if /landing is requested and landing.html exists,
		// serve landing.html with revalidation headers so client routes do not flash landing markup.
		if reqPath == "landing" {
			if _, err := fs.Stat(sub, "landing.html"); err == nil {
				c.Header("Cache-Control", "no-cache, must-revalidate")
				c.Request.URL.Path = "/landing.html"
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
		}

		fileExists := func() bool {
			_, err := fs.Stat(sub, reqPath)
			return err == nil
		}

		if !fileExists() {
			// A path containing a dot is an asset reference (JS, CSS, PNG, etc.).
			// If it does not exist in the embed it is stale (old deploy). Return 404
			// so the browser does not execute index.html as JavaScript.
			if strings.Contains(reqPath, ".") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}

			// Never fall back to index.html for API routes or non-GET/HEAD methods.
			if strings.HasPrefix(reqPath, "api/") || reqPath == "api" || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
					"error": "route not found",
					"code":  "route.notFound",
				})
				return
			}

			// Client-side route fallback → index.html with no-cache so the browser
			// always revalidates before using it (prevents stale shell after deploy).
			c.Header("Cache-Control", "no-cache, must-revalidate")
			c.Request.URL.Path = "/"
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}

		// File exists — set appropriate cache headers before serving.
		switch {
		case hashedAssetRE.MatchString(reqPath):
			// Content-hashed assets are immutable: safe to cache for 1 year.
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		case reqPath == "index.html" || reqPath == "landing.html" || reqPath == "sw.js" || reqPath == "service-worker.js":
			// Entry point, landing page and service worker must always revalidate.
			c.Header("Cache-Control", "no-cache, must-revalidate")
		}

		// Never fall back to index.html for API routes or non-GET/HEAD methods
		// (redundant guard — kept for clarity since fileExists() passed above).
		if strings.HasPrefix(reqPath, "api/") || reqPath == "api" || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "route not found",
				"code":  "route.notFound",
			})
			return
		}

		fileServer.ServeHTTP(c.Writer, c.Request)
	}
}
