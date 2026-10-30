package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 分词器可信基线的管理接口（仅管理员）。
//
// 录制本身走普通检测队列（勾选「录制分词器基线」那一项，在**你确认可信**的渠道上跑），
// 这里只负责把一条已完成的录制结果提升为基线、列出、删除。不接受手填计数。
func init() {
	// 探针读本地基线走持久层；不注册时探针只用内置库。
	behavior.SetTokenizerBaselineSource(op.TokenizerBaselineLocals)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/tokenizer-baselines", http.MethodGet).
				Handle(listTokenizerBaselines),
		).
		AddRoute(
			router.NewRoute("/tokenizer-baselines/:id", http.MethodDelete).
				Handle(deleteTokenizerBaseline),
		)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/tokenizer-baselines", http.MethodPost).
				Handle(createTokenizerBaseline),
		)
}

func listTokenizerBaselines(c *gin.Context) {
	rows, err := op.TokenizerBaselineList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"baselines": rows})
}

type tokenizerBaselineCreateRequest struct {
	ResultID int64  `json:"result_id"`
	Label    string `json:"label"`
	Replace  bool   `json:"replace"`
}

func createTokenizerBaseline(c *gin.Context) {
	var req tokenizerBaselineCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.ResultID <= 0 {
		resp.Error(c, http.StatusBadRequest, "result_id is required")
		return
	}
	row, err := op.TokenizerBaselineFromResult(c.Request.Context(), op.TokenizerBaselineFromResultInput{
		ResultID: req.ResultID, Label: req.Label, Replace: req.Replace,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, op.ErrTokenizerBaselineExists) {
			status = http.StatusConflict
		}
		resp.Error(c, status, err.Error())
		return
	}
	resp.Success(c, gin.H{"baseline": row})
}

func deleteTokenizerBaseline(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid baseline id")
		return
	}
	if err := op.TokenizerBaselineDelete(c.Request.Context(), id); err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	resp.Success(c, gin.H{"id": id})
}
