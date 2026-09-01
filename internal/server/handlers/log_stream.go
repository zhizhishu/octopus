package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/utils/safe"
	"github.com/gin-gonic/gin"
)

// LogStreamSSE 提供实时请求状态的 SSE 推送端点。
// 客户端连接后立即收到当前所有 running/finished 状态的快照，
// 然后持续接收增量更新，直到客户端断开或服务关闭。
func LogStreamSSE(c *gin.Context) {
	token := c.Query("token")
	tokenScope, ok := op.RelayLogStreamTokenVerify(token)
	if token == "" || !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "invalid stream token"})
		return
	}
	op.RelayLogStreamTokenRevoke(token)

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // 禁用 nginx 缓冲

	// 订阅请求状态更新
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	stateChan := relay.SubscribeRequestState(ctx)
	visibleState := func(state *relay.RequestState) bool {
		return tokenScope.IsAdmin || (state != nil && state.UserID == tokenScope.UserID)
	}
	defer relay.UnsubscribeRequestState(stateChan)

	// 确保响应立即 flush（Gin 默认会缓冲）
	c.Writer.Flush()

	// 心跳 ticker：每 15 秒发送一次注释行，防止代理超时
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// 持续推送状态更新
	safe.SafeGo("log-stream-sse", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.Request.Context().Done():
				return
			case state, ok := <-stateChan:
				if !ok {
					return
				}
				if !visibleState(state) {
					continue
				}
				// 序列化状态为 JSON
				data, err := json.Marshal(state)
				if err != nil {
					continue
				}
				// 发送 SSE 事件
				fmt.Fprintf(c.Writer, "data: %s\n\n", data)
				c.Writer.Flush()
			case <-heartbeat.C:
				// 发送心跳注释
				fmt.Fprintf(c.Writer, ": heartbeat\n\n")
				c.Writer.Flush()
			}
		}
	})

	// 阻塞直到客户端断开
	<-c.Request.Context().Done()
}

// LogSnapshotJSON 提供当前所有请求状态的 JSON 快照（供轮询客户端使用）。
func LogSnapshotJSON(c *gin.Context) {
	snapshot := relay.GetRequestStateSnapshotForUser(middleware.CurrentUserID(c), middleware.CurrentUserIsAdmin(c))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    snapshot,
	})
}
