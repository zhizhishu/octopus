package middleware

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func StaticEmbed(urlPrefix string, embedFS fs.FS) gin.HandlerFunc {
	fs := http.FS(embedFS)
	return static(urlPrefix, fs)
}

func StaticLocal(urlPrefix string, localPath string) gin.HandlerFunc {
	fs := http.Dir(localPath)
	return static(urlPrefix, fs)
}

func static(urlPrefix string, fileSystem http.FileSystem) gin.HandlerFunc {
	fileserver := http.FileServer(fileSystem)
	if urlPrefix != "" {
		fileserver = http.StripPrefix(urlPrefix, fileserver)
	}
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.Next()
			return
		}
		if _, err := fileSystem.Open(c.Request.URL.Path); err == nil {
			// 内容哈希过的构建产物（Next.js `/_next/static/*`）文件名随每次构建变，
			// 可安全长缓存 immutable；其余（index.html / HTML / 未哈希资源）必须
			// no-cache 每次重验——否则浏览器把 index.html 也当 immutable 死缓存一年、
			// 一直加载旧 bundle，部署后“拉了也不生效”（配 http.FileServer 的
			// ETag/If-Modified-Since 走 304，重验开销极小）。
			if strings.HasPrefix(c.Request.URL.Path, "/_next/static/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				c.Header("Cache-Control", "no-cache")
			}
			fileserver.ServeHTTP(c.Writer, c.Request)
			c.Abort()
		}
	}
}
