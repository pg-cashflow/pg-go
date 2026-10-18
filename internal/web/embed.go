package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// dist/ holds the built pg-react assets.
// A committed dist/index.html guarantees clean checkouts compile and
// pass `go test ./...` even before the frontend has ever been built.
//
//go:embed dist/*
var distFS embed.FS

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

		// If real file exists in dist (e.g. /favicon.ico, /assets/main.js), serve it
		if _, err := fs.Stat(sub, reqPath); err == nil {
			fileServer.ServeHTTP(c.Writer, c.Request)
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

		// Client-side route fallback -> index.html
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	}
}
