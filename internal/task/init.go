package task

import (
	"context"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

const (
	TaskPriceUpdate      = "price_update"
	TaskStatsSave        = "stats_save"
	TaskRelayLogSave     = "relay_log_save"
	TaskUserRelayIPSave  = "user_relay_ip_save"
	TaskChannelKeySave   = "channel_key_save"
	TaskMaintenanceSweep = "maintenance_sweep"
	TaskSyncLLM          = "sync_llm"
	TaskCleanLLM         = "clean_llm"
	TaskBaseUrlDelay     = "base_url_delay"
	TaskModelAuditQuick  = "model_audit_quick"
)

// settingIntOr 读一个整数设置，读不到就用默认值继续。
//
// 🔴 这里以前是 `if err != nil { return }` —— 三处 early-return 会把**后面所有任务
// 的注册整段跳过**。SettingGetInt 只在 settingCache 里查，key 缺失、值是空串、
// 或写成 "5.5"/"5 分钟" 都会返回 err，于是：
//   - TaskStatsSave / TaskRelayLogSave 不注册 → 统计数据永不落库
//   - price_update 不注册 → 模型价格永不更新
//   - base_url_delay 不注册 → 渠道延迟永不刷新
//   - sync_llm 不注册 → 模型列表永不同步
// 而 relayLogFlushLoop 是独立协程仍在跑，日志看着还在落库，**故障极难被发现**：
// 服务正常收发请求，只有启动时一行 Error。
//
// 一个配错的值不该让整台机器半瘫。读不到就用默认值，并把问题喊出来。
func settingIntOr(key model.SettingKey, def int) int {
	v, err := op.SettingGetInt(key)
	if err != nil {
		log.Errorf("setting %q unreadable (%v); falling back to default %d and continuing task registration", key, err, def)
		return def
	}
	if v <= 0 {
		log.Errorf("setting %q is %d (invalid); falling back to default %d", key, v, def)
		return def
	}
	return v
}

func Init() {
	// 先启动异步落库者, 再读任何可能 early-return 的设置。中继日志后台刷库协程 + relay IP
	// 审计落库任务负责排空转发热路径塞进内存的两个队列; 若排在下面的 SettingGetInt 读失败
	// 直接 return, 它们必须已经在跑, 否则中继日志会一路涨到 pending 上限被静默丢弃、
	// last_relay_ip/at 永不落库(旧的同步写没有这个启动依赖, 是改异步后新引入的脆弱点)。
	// 显式从这里起而不是 RelayLogAdd 里惰性起——生产必经此路, 而测试二进制绝不该有无人管
	// 生命周期的后台写库者(会撞各用例的临时 DB, CI 实翻过)。
	op.RelayLogFlusherStart()
	// 视频任务轮询器同理: 它每一轮都读库写库, 绝不许在包里靠 init() 自启(见上面那段注释,
	// 也见 VideoTaskPollStart 的注释)。放在这里而不是放到下面的 Register 序列里, 有两个理由:
	//  1) 视频任务的状态推进和设置读取无关, 不能因为某个 SettingGetInt 失败 early-return
	//     就整条链路停摆——那种情况下已经提交的任务会永远停在 submitted;
	//  2) 它是自带 ticker 的独立循环(sync.Once + safe.SafeGo), 不进 task 注册表,
	//     所以测试二进制里只要没人显式调用它, 就不会有任何后台写库者存在。
	VideoTaskPollStart()
	// 模型行为检测(定时快检 + 回显不一致自动跟进)已停用自动启动: 此处既不挂 RegisterAuditFollowUp,
	// 也不注册 TaskModelAuditQuick——后台与日志写入路径不得自动启动消耗 token 的 behavior 探针。
	// 只有检测栏手动 run(POST /api/v1/model-audit/run) 仍可主动调用; 两个旧开关默认 false, 且因
	// 不再接线, 即使部署里残留 true 也不会再自动跑。
	Register(TaskUserRelayIPSave, 1*time.Minute, false, op.UserRelayIPSaveDBTask)

	// 渠道 key 的计费(TotalCost)/401 隔离态(DisabledReason/At)/末次使用原本只在优雅退出的
	// SaveCache 里落库, 崩溃/OOM/docker kill 会丢掉自启动以来累计的全部渠道 key 账目(含把坏
	// key 重新放行的隐患)。周期补一刀, 把丢失窗口从"整个进程生命周期"收敛到一个周期。同样放在
	// 价格设置读取之前, 免得那处 early-return 把它一起跳过。
	Register(TaskChannelKeySave, 5*time.Minute, false, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := op.ChannelKeySaveDB(ctx); err != nil {
			log.Warnf("channel key save db task failed: %v", err)
		}
	})

	// 周期内存/DB 维护清扫: 把几处"只惰性清、静默期不清"的表定期收一遍——粘性会话(惰性删只在
	// 命中时触发)、responses-session/transcript(仅 record 时剪)、日志流令牌(创建后未消费无 TTL),
	// 以及 response-session 过期绑定的 DB 剪枝(原内联在请求路径且吞错误)。只删确实过期/够旧的项、
	// 不动活数据。放在设置读取之前, 同样避免那处 early-return 把它跳过。
	Register(TaskMaintenanceSweep, 5*time.Minute, false, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := op.ResponseSessionPruneExpired(ctx); err != nil {
			log.Warnf("response session prune failed: %v", err)
		}
		balancer.SweepExpired(24 * time.Hour)
		relay.PruneResponsesSessionsExpired()
		op.RelayLogStreamTokenSweep(time.Hour)
	})

	priceUpdateInterval := time.Duration(settingIntOr(model.SettingKeyModelInfoUpdateInterval, 24)) * time.Hour
	// 注册价格更新任务
	Register(string(model.SettingKeyModelInfoUpdateInterval), priceUpdateInterval, true, func() {
		if err := price.UpdateLLMPrice(context.Background()); err != nil {
			log.Warnf("failed to update price info: %v", err)
		}
	})

	// 注册基础URL延迟任务
	Register(TaskBaseUrlDelay, 1*time.Hour, true, ChannelBaseUrlDelayTask)

	// 注册LLM同步任务
	syncLLMInterval := time.Duration(settingIntOr(model.SettingKeySyncLLMInterval, 24)) * time.Hour
	Register(string(model.SettingKeySyncLLMInterval), syncLLMInterval, true, SyncModelsTask)

	// 注册统计保存任务
	statsSaveInterval := time.Duration(settingIntOr(model.SettingKeyStatsSaveInterval, 5)) * time.Minute
	Register(TaskStatsSave, statsSaveInterval, false, op.StatsSaveDBTask)
	// 注册中继日志保存任务
	Register(TaskRelayLogSave, 10*time.Minute, false, func() {
		if err := op.RelayLogSaveDBTask(context.Background()); err != nil {
			log.Warnf("relay log save db task failed: %v", err)
		}
	})
}
