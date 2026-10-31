package relay

import (
	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
)

// 访客 Key 在转发路径上只多两条规矩，都只管「入站这一侧」——不碰选路、不碰出站请求
// （cloak / shape / 头 / 顺序 / TLS / 请求体一个字节都不动）：
//
//  1. 任务归属只认 Key：访客 Key 的 user_id 是管理员，按「同一用户」放行会让它看到管理员
//     名下的旧任务（只记了 user、没记 token 的那批）。所以查任务 / 轮询 / 取结果时，
//     访客 Key 的「请求方用户」按 0 算，只有自己这把 Key 提交的才是它的。
//  2. 计费口径按「Key 上限」收紧：管理员本身不扣余额（自用模式放行没价模型、长连接不中途断），
//     但访客 Key 有自己的 MaxCost，所以没价的异步图片不受理、实时语音按 Key 上限中途结算断开。

func isGuestKeyRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(model.APIKeyGuestContextKey)
}

// taskOwnerUserID 是做「任务归属」判断时请求方的 user_id：访客 Key 恒为 0（只按 Key 匹配）。
func taskOwnerUserID(c *gin.Context) int {
	if isGuestKeyRequest(c) {
		return 0
	}
	return c.GetInt("user_id")
}
