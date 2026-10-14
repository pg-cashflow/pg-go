package web

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Handler serves the frozen shell under the /app URL prefix.
func Handler() gin.HandlerFunc {
	sub, err := fs.Sub(FS, ".")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if !strings.HasPrefix(p, "/app") {
			c.Next()
			return
		}
		if p == "/app" {
			c.Redirect(http.StatusMovedPermanently, "/app/")
			c.Abort()
			return
		}
		rel := strings.TrimPrefix(p, "/app/")
		if rel == "" || rel == "/" {
			rel = "index.html"
		}
		c.Request.URL.Path = "/" + rel
		fileServer.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}
}
