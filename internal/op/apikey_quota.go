package op

import (
	"errors"
	"sync"

	"github.com/zhizhishu/octopus/internal/model"
)

// API Key 花费上限（MaxCost）的「检查 + 在途预留」，照 UserCanRelay 的做法。
//
// 以前只判「已完成请求的累计花费 < 上限」：同时发很多请求、或者一次提交很多异步出图任务，
// 在任何一笔落账之前全都能过闸，最后花的钱可以远超上限。访客 Key 归属管理员、不扣余额，
// 上限是它唯一的闸，所以这个漏洞对它影响最大；普通 Key 设了上限也是同一个口径。
//
// 做法：每把 Key 一个内存态在途额度。准入时在锁内一次完成
// 「已花 + 在途 >= 上限 → 拒绝；否则占一笔」，请求结束（或异步任务终态）时归还。
// 进程重启后没有在途请求，清零即正确（与 userQuotaInflight 同一语义）。
// 没设上限（MaxCost <= 0）的 Key 不占、不拦。

// ErrAPIKeyMaxCost：Key 的花费上限用完了（含在途）。
var ErrAPIKeyMaxCost = errors.New("API key has reached the max cost")

var (
	apiKeyQuotaMu       sync.Mutex
	apiKeyQuotaInflight = make(map[int]float64)
)

// APIKeySpent 是这把 Key 已经落账的累计花费（与中间件、实时语音同一个判据）。
func APIKeySpent(apiKeyID int) float64 {
	s := StatsAPIKeyGet(apiKeyID)
	return s.StatsMetrics.InputCost + s.StatsMetrics.OutputCost
}

// APIKeyReserve 为一次请求（或一条异步任务）在 Key 的上限里占一笔。
//
// amount <= 0：按 UserCanRelay 的默认量子占（userQuotaReserveOf）；
// amount > 0：已知要花多少（例如按张计费的异步出图），剩余不够就直接拒绝。
// 返回实际占的金额（没设上限时为 0），必须交给 APIKeyReleaseReserve 归还。
func APIKeyReserve(key model.APIKey, amount float64) (float64, error) {
	if key.ID <= 0 || key.MaxCost <= 0 {
		return 0, nil
	}
	apiKeyQuotaMu.Lock()
	defer apiKeyQuotaMu.Unlock()
	inflight := apiKeyQuotaInflight[key.ID]
	available := key.MaxCost - APIKeySpent(key.ID) - inflight
	if available <= userQuotaInflightEpsilon {
		return 0, ErrAPIKeyMaxCost
	}
	reservation := amount
	if reservation <= 0 {
		reservation = userQuotaReserveOf(available)
	} else if reservation > available+userQuotaInflightEpsilon {
		return 0, ErrAPIKeyMaxCost
	}
	apiKeyQuotaInflight[key.ID] = inflight + reservation
	return reservation, nil
}

// APIKeyReleaseReserve 归还 APIKeyReserve 占的那一笔。重复 / 多余的归还夹到 0。
func APIKeyReleaseReserve(apiKeyID int, reservation float64) {
	if apiKeyID <= 0 || reservation <= 0 {
		return
	}
	apiKeyQuotaMu.Lock()
	defer apiKeyQuotaMu.Unlock()
	remaining := maxFloat(apiKeyQuotaInflight[apiKeyID]-reservation, 0)
	if remaining <= userQuotaInflightEpsilon {
		delete(apiKeyQuotaInflight, apiKeyID)
		return
	}
	apiKeyQuotaInflight[apiKeyID] = remaining
}

// APIKeyInflight 是这把 Key 此刻所有在途预留的合计（长连接算「剩多少」时扣掉别人的占用）。
func APIKeyInflight(apiKeyID int) float64 {
	apiKeyQuotaMu.Lock()
	defer apiKeyQuotaMu.Unlock()
	return apiKeyQuotaInflight[apiKeyID]
}
