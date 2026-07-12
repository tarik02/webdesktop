package web

import (
	"io/fs"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/gin-gonic/gin"
)

// Mount serves the embedded production SPA without replacing API routes.
func Mount(router *gin.Engine) {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(dist))

	router.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		requestPath := strings.TrimPrefix(pathpkg.Clean("/"+c.Request.URL.Path), "/")
		if requestPath == "" {
			requestPath = "index.html"
		}
		if strings.HasPrefix(requestPath, "api/") || requestPath == "healthz" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if _, err := fs.Stat(dist, requestPath); err != nil {
			if strings.Contains(pathpkg.Base(requestPath), ".") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			requestPath = "index.html"
		}

		if requestPath == "index.html" {
			c.Header("Cache-Control", "no-cache")
			c.Data(http.StatusOK, "text/html; charset=utf-8", index)
			return
		}
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		request := c.Request.Clone(c.Request.Context())
		request.URL.Path = "/" + requestPath
		fileServer.ServeHTTP(c.Writer, request)
	})
}
