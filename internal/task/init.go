package task

import (
	"context"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/utils/log"
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
)

func Init() {
	// 先启动异步落库者, 再读任何可能 early-return 的设置。中继日志后台刷库协程 + relay IP
	// 审计落库任务负责排空转发热路径塞进内存的两个队列; 若排在下面的 SettingGetInt 读失败
	// 直接 return, 它们必须已经在跑, 否则中继日志会一路涨到 pending 上限被静默丢弃、
	// last_relay_ip/at 永不落库(旧的同步写没有这个启动依赖, 是改异步后新引入的脆弱点)。
	// 显式从这里起而不是 RelayLogAdd 里惰性起——生产必经此路, 而测试二进制绝不该有无人管
	// 生命周期的后台写库者(会撞各用例的临时 DB, CI 实翻过)。
	op.RelayLogFlusherStart()
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

	priceUpdateIntervalHours, err := op.SettingGetInt(model.SettingKeyModelInfoUpdateInterval)
	if err != nil {
		log.Errorf("failed to get model info update interval: %v", err)
		return
	}
	priceUpdateInterval := time.Duration(priceUpdateIntervalHours) * time.Hour
	// 注册价格更新任务
	Register(string(model.SettingKeyModelInfoUpdateInterval), priceUpdateInterval, true, func() {
		if err := price.UpdateLLMPrice(context.Background()); err != nil {
			log.Warnf("failed to update price info: %v", err)
		}
	})

	// 注册基础URL延迟任务
	Register(TaskBaseUrlDelay, 1*time.Hour, true, ChannelBaseUrlDelayTask)

	// 注册LLM同步任务
	syncLLMIntervalHours, err := op.SettingGetInt(model.SettingKeySyncLLMInterval)
	if err != nil {
		log.Warnf("failed to get sync LLM interval: %v", err)
		return
	}
	syncLLMInterval := time.Duration(syncLLMIntervalHours) * time.Hour
	Register(string(model.SettingKeySyncLLMInterval), syncLLMInterval, true, SyncModelsTask)

	// 注册统计保存任务
	statsSaveIntervalMinutes, err := op.SettingGetInt(model.SettingKeyStatsSaveInterval)
	if err != nil {
		log.Warnf("failed to get stats save interval: %v", err)
		return
	}
	statsSaveInterval := time.Duration(statsSaveIntervalMinutes) * time.Minute
	Register(TaskStatsSave, statsSaveInterval, false, op.StatsSaveDBTask)
	// 注册中继日志保存任务
	Register(TaskRelayLogSave, 10*time.Minute, false, func() {
		if err := op.RelayLogSaveDBTask(context.Background()); err != nil {
			log.Warnf("relay log save db task failed: %v", err)
		}
	})
}
