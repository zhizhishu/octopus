package op

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 抽检的每日预算账本。
//
// 为什么需要它：抽检（定时抽样 + 可疑日志跟查）发出去的每一条题目都是一次真实、
// 计费的上游请求。之前的开关只能决定"打不打"，决定不了"打多少"——一个配错、
// 或者几十个渠道同时开着的部署，一夜之间就能把额度烧光，而日志上只有一堆检测结果，
// 没有任何地方说得出"今天到底花了多少次请求"。
//
// 这里只做一件事：把「今天已经用了多少次」记在设置表里，并在每次真正入队之前
// 先扣一次额度。扣不到就不发。
//
// 几个刻意的取舍：
//   - 账本按自然日归零（now.In(time.Local) 的 YYYY-MM-DD），跨天不需要任何清理任务；
//     日期对不上就当今天还没用——多花的可能性远小于"跨天之后再也跑不了"的代价。
//   - 扣额度失败（写不进设置表）就不发请求：记不住的账等于没上限，宁可不跑这一轮。
//   - 只扣"计划请求数"（behavior.PlannedRequests），不是实际发包数：探针会在中途
//     提前结束，实际用量只会小于等于计划量，所以这是一个保守的上界。

// modelAuditBudgetDefaultLimit 全站每日上游请求上限的兜底值。
// 设置读不出来或填了 ≤0 时用它——预算闸门不允许因为一次读配置抖动而消失。
// （出厂种子值在 model.DefaultSettings 里，同样是 200；两处一致由
// op/model_audit_budget_test.go 的 TestModelAuditBudgetLimitFallsBackTo200 对齐。）
const modelAuditBudgetDefaultLimit = 200

// modelAuditBudgetUsage 是账本落库形态。
//
// Date 存 YYYY-MM-DD 字符串而不是时间戳：它的唯一用途就是和"今天"比较，存成字符串
// 后在库里/设置页都是人可读的，也没有时区换算的余地。
type modelAuditBudgetUsage struct {
	Date     string `json:"date"`
	Requests int    `json:"requests"`
}

// modelAuditBudgetMu 串行化「读账本 → 判断 → 写账本」。
//
// 与 settingWriteMu 是两把锁、两层保护：那把锁保护的是设置表的写入一致性，这把锁
// 保护的是"额度不会被并发超发"。两次 Reserve 交错执行时必须看到彼此的结果，
// 否则 N 个 goroutine 各自读到 used=0 然后各扣一次，上限形同虚设。
var modelAuditBudgetMu sync.Mutex

// ModelAuditBudgetReserve 为一次计划入队（planned 次上游请求）预扣当日额度。
//
// 返回：
//   - ok：是否扣成功。false 表示今天已经不够用了，调用方必须**不要**发请求；
//   - used：账本上的用量（扣成功时是扣后的值，扣失败时是当前值，供提示文案使用）；
//   - limit：当日的上限（读不到/≤0 时按 200）。
func ModelAuditBudgetReserve(planned int, now time.Time) (ok bool, used, limit int) {
	modelAuditBudgetMu.Lock()
	defer modelAuditBudgetMu.Unlock()

	limit = modelAuditBudgetLimit()
	usage := modelAuditBudgetRead(now)
	if planned < 0 {
		planned = 0
	}
	if usage.Requests+planned > limit {
		return false, usage.Requests, limit
	}
	usage.Requests += planned
	if !modelAuditBudgetWrite(usage) {
		// 记不住就不发：账本写不进去时要当"没扣成功"，否则这一段用量永远算不进上限。
		return false, usage.Requests - planned, limit
	}
	return true, usage.Requests, limit
}

// ModelAuditBudgetStatus 返回"今天"的用量与上限（纯读，不扣额度）。
func ModelAuditBudgetStatus(now time.Time) (used, limit int) {
	modelAuditBudgetMu.Lock()
	defer modelAuditBudgetMu.Unlock()
	usage := modelAuditBudgetRead(now)
	return usage.Requests, modelAuditBudgetLimit()
}

// modelAuditBudgetLimit 读当日上限。读不到、不是整数、或 ≤0 一律按 200。
//
// 抽检的钱包闸门不允许"读配置失败就变成无限额"，所以这里有一层硬兜底，
// 与 RelayLogAdd 那种"读失败按开"的零成本路径正好相反：会花钱的东西读失败按最保守算。
func modelAuditBudgetLimit() int {
	value, err := SettingGetInt(model.SettingKeyModelAuditDailyRequestLimit)
	if err != nil || value <= 0 {
		return modelAuditBudgetDefaultLimit
	}
	return value
}

// modelAuditBudgetRead 读"当天"的用量。日期不是今天（含读不到/解析失败）一律当 0。
func modelAuditBudgetRead(now time.Time) modelAuditBudgetUsage {
	today := modelAuditBudgetDate(now)
	raw, err := SettingGetString(model.SettingKeyModelAuditBudgetUsage)
	if err != nil {
		return modelAuditBudgetUsage{Date: today}
	}
	var usage modelAuditBudgetUsage
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		return modelAuditBudgetUsage{Date: today}
	}
	if usage.Date != today {
		// 跨天归零：不需要任何清理任务，日期对不上就是"今天还没用"。
		return modelAuditBudgetUsage{Date: today}
	}
	if usage.Requests < 0 {
		usage.Requests = 0
	}
	return usage
}

// modelAuditBudgetWrite 把账本写回设置表。返回是否写成功（调用方必须把它当条件用）。
func modelAuditBudgetWrite(usage modelAuditBudgetUsage) bool {
	if usage.Date == "" {
		return false
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return false
	}
	return SettingSetString(model.SettingKeyModelAuditBudgetUsage, string(raw)) == nil
}

// modelAuditBudgetDate 是账本的"哪一天"口径：容器本地时区的自然日。
func modelAuditBudgetDate(now time.Time) string {
	return now.In(time.Local).Format("2006-01-02")
}
