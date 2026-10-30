package handlers

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/logimage"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 「真实出站请求体」里另存的原图。可见范围与那一栏正文一致：只给管理员
// （非管理员连正文都拿不到，见 op.RelayLogRedact）。
func init() {
	router.NewGroupRouter("/api/v1/log").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/image/:hash", http.MethodGet).
				Handle(getLogImage),
		)
}

func getLogImage(c *gin.Context) {
	hash := strings.TrimSpace(c.Param("hash"))
	// 先校验再拼路径：只有 64 位小写十六进制能走到文件系统，../ 之类根本进不来。
	if !logimage.ValidHash(hash) {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	path, ext, ok := logimage.Find(hash)
	if !ok {
		resp.Error(c, http.StatusNotFound, "image not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		resp.Error(c, http.StatusNotFound, "image not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		resp.Error(c, http.StatusNotFound, "image not found")
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Type", logimage.MIMEForExt(ext))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `inline; filename="`+hash+"."+ext+`"`)
	// 按内容寻址，同一个地址的内容永远不变；但它是带鉴权的私有数据。
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(c.Writer, c.Request, "", st.ModTime(), f)
}
