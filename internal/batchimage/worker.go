package batchimage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 后台推进器：把已经落库的批次真正推到上游，并一路跟到终态。
//
// 【为什么必须有它】提交路径不回上游（见 Service.Submit 的注释）。所有真正的
// 上游交互都发生在这里，而这里是**唯一**的写者，所以并发问题从"三个写者抢一条
// 任务"缩小到"同一进程内的多个 worker 抢同一条任务"——用 CAS 就够。
//
// 【钱的铁律】任何一条任务到了终态，第一件事是结算（Service.Settle）。
// 结算本身是幂等的（抢 settled_at），所以这里宁可重复调用也不要有分支绕过它。

// 推进器的运行参数。
const (
	// workerInterval 是两轮之间的间隔。
	workerInterval = 10 * time.Second
	// workerBatchLimit 是单轮最多处理多少条任务。
	workerBatchLimit = 20
	// workerExpiredLimit 是单轮最多收口多少条过期任务。
	workerExpiredLimit = 50
	// workerConcurrency 是单轮之内并行处理的任务数。
	//
	// 4 是个折中：串行处理会让一批卡住的提交拖住所有其它任务，
	// 而并发太高会把上游的限流打成一片 429。
	workerConcurrency = 4
	// workerRunBudget 是单轮的时间预算。
	workerRunBudget = 2 * time.Minute
	// workerTaskTimeout 是单条任务的处理超时。
	workerTaskTimeout = 3 * time.Minute
)

// Worker 是推进器。
type Worker struct {
	Service *Service
	// Settings 与 Service.Settings 同源（转一层是为了 worker 自己也用得上）。
	Settings func(ctx context.Context) Settings
	// Poll 取上游进度。抽成函数是为了测试能塞一个可控的假上游。
	ProviderFor func(channel *model.Channel) (Provider, error)

	// stopped 用于让 Start 能在测试里干净停下。
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewWorker 造一个推进器。**不会**自己开始跑：调用方必须显式 Start。
//
// 【为什么不自启】见 internal/task/image_task_poll.go 顶部那段：任何 init() 自注册
// 都会让每个 go test 二进制在跑测试时启动一个没人管的数据库写入器，
// 它会给真实用户扣钱。这个决定不是洁癖，是有代价的事故预防。
func NewWorker(service *Service) *Worker {
	return &Worker{
		Service: service,
		stopCh:  make(chan struct{}),
	}
}

// Start 启动后台循环。重复调用是安全的（第二次什么都不做）。
func (w *Worker) Start() {
	if w == nil || w.Service == nil {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(workerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stopCh:
				return
			case <-ticker.C:
				// 单轮不能把进程拖死：给一个预算，超了就等下一轮。
				ctx, cancel := context.WithTimeout(context.Background(), workerRunBudget)
				w.RunOnce(ctx)
				cancel()
			}
		}
	}()
}

// Stop 停止后台循环并等它退出。
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	w.wg.Wait()
}

// RunOnce 跑一轮。导出是为了让测试能显式驱动它（不依赖 ticker）。
func (w *Worker) RunOnce(ctx context.Context) {
	if w == nil || w.Service == nil {
		return
	}
	settings := w.settings(ctx)
	if !settings.Enabled {
		// 关掉时**什么都不做**：既不起新任务，也不收口在途任务。
		// 在途任务留在库里，下次打开会继续——这比"关的时候把所有在途判失败"
		// 仁慈得多，也不会因为运维关了一下开关就退一片钱。
		return
	}

	w.expire(ctx)
	w.advance(ctx)
}

// settings 取配置。
func (w *Worker) settings(ctx context.Context) Settings {
	if w.Settings != nil {
		return w.Settings(ctx).Normalize()
	}
	if w.Service.Settings != nil {
		return w.Service.Settings(ctx).Normalize()
	}
	return Settings{}.Normalize()
}

// expire 收口已经过了保留期的任务。
func (w *Worker) expire(ctx context.Context) {
	jobs, err := op.BatchImageListExpired(ctx, workerExpiredLimit)
	if err != nil {
		log.Errorf("batch image worker: list expired jobs: %v", err)
		return
	}
	for index := range jobs {
		job := jobs[index]
		if err := w.expireOne(ctx, &job); err != nil {
			log.Errorf("batch image worker: expire job %s: %v", job.BatchID, err)
		}
	}
}

// expireOne 收口一条过期任务。
func (w *Worker) expireOne(ctx context.Context, job *model.BatchImageJob) error {
	switch job.Status {
	case model.BatchImageJobStatusCompleted:
		// 已经完成但产物还在：保留期到了就标记产物已删。
		// **钱不动**——这一单早就结清了。
		if _, err := op.BatchImageMarkOutputDeleted(ctx, job.ID); err != nil {
			return err
		}
		// 顺手清上游残留（输入文件），失败不影响状态。
		w.cleanupUpstream(ctx, job, false, true)
		return nil

	case model.BatchImageJobStatusQueued, model.BatchImageJobStatusSubmitted, model.BatchImageJobStatusIndexing:
		// 还没跑完就过期了：标失败、退款、尽力撤单。
		//
		// 【为什么标失败而不是取消】取消是客户端主动行为；过期是我们自己没收口，
		// 对用户来说"我们没能在保留期内跑完"就是失败，如实说。
		if _, err := op.BatchImageStatusCAS(ctx, job.ID, job.Status,
			model.BatchImageJobStatusFailed, map[string]any{
				"error_code":    "batch_image_expired",
				"error_message": "the job did not finish within its retention window",
				"completed_at":  time.Now().Unix(),
			}); err != nil {
			return err
		}
		job.Status = model.BatchImageJobStatusFailed
		if _, err := w.Service.Settle(ctx, job); err != nil {
			log.Errorf("batch image worker: settle expired job %s: %v", job.BatchID, err)
		}
		// 尽力撤上游，避免它继续烧我们的额度。
		w.cancelUpstream(ctx, job)
		w.cleanupUpstream(ctx, job, true, false)
		return nil
	}
	return nil
}

// advance 推进一批在途任务。
func (w *Worker) advance(ctx context.Context) {
	jobs, err := op.BatchImageListExecutable(ctx, workerBatchLimit)
	if err != nil {
		log.Errorf("batch image worker: list executable jobs: %v", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	semaphore := make(chan struct{}, workerConcurrency)
	var wg sync.WaitGroup
	for index := range jobs {
		job := jobs[index]
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			taskCtx, cancel := context.WithTimeout(ctx, workerTaskTimeout)
			defer cancel()
			if err := w.advanceOne(taskCtx, &job); err != nil {
				log.Errorf("batch image worker: advance job %s: %v", job.BatchID, err)
			}
		}()
	}
	wg.Wait()
}

// advanceOne 按当前状态推进一步。
func (w *Worker) advanceOne(ctx context.Context, job *model.BatchImageJob) error {
	provider, channel, channelKey, err := w.resolve(ctx, job)
	if err != nil {
		// 渠道没了/没配好：这不是上游的事，是我们的配置问题。
		// 标失败并退款，别让它挂在队列里每轮报同一个错。
		return w.abort(ctx, job, "batch_image_channel_unavailable", err)
	}
	channelCtx := channelContextOf(channel, channelKey)

	switch job.Status {
	case model.BatchImageJobStatusQueued:
		return w.submitOne(ctx, job, provider, channelCtx)
	case model.BatchImageJobStatusSubmitted:
		return w.pollOne(ctx, job, provider, channelCtx)
	case model.BatchImageJobStatusIndexing:
		return w.indexOne(ctx, job, provider, channelCtx)
	}
	return nil
}

// submitOne 把一条 queued 的任务交给上游。
func (w *Worker) submitOne(ctx context.Context, job *model.BatchImageJob, provider Provider, channel ChannelContext) error {
	// 重建 JSONL。
	items, err := w.loadItems(ctx, job)
	if err != nil {
		return w.abort(ctx, job, "batch_image_items_unreadable", err)
	}
	jsonl, err := BuildJSONL(BuildRequest{
		Items:            items,
		ResponseMimeType: job.ResponseMimeType,
		AspectRatio:      job.AspectRatio,
		ImageSize:        job.ImageSize,
	})
	if err != nil {
		// 构建不出来是我们自己的问题（提交时已经校验过一次），
		// 说明库里的数据与提交时不一致。标失败退款，让用户重新提交。
		return w.abort(ctx, job, "batch_image_build_failed", err)
	}

	result, err := provider.Submit(ctx, SubmitRequest{
		Channel:     channel,
		BatchID:     job.BatchID,
		Model:       job.Model,
		DisplayName: job.BatchID,
		JSONL:       jsonl,
		Items:       items,
	})
	if err != nil {
		var upstream *upstreamError
		if errors.As(err, &upstream) && upstream.Temporary() {
			// 可重试的错误：保持 queued，下一轮再来。不动钱。
			return fmt.Errorf("temporary submit failure, will retry: %w", err)
		}
		return w.abort(ctx, job, "batch_image_submit_failed", err)
	}

	// queued → submitted。
	advanced, err := op.BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusQueued, model.BatchImageJobStatusSubmitted, map[string]any{
			"provider_job_name":   result.ProviderJobName,
			"provider_input_ref":  result.ProviderInputRef,
			"provider_output_ref": result.ProviderOutputRef,
			"provider_raw_state":  result.RawState,
			"submitted_at":        time.Now().Unix(),
		})
	if err != nil {
		return err
	}
	if !advanced {
		// 别人已经推进过了（客户端刚好取消）。这不是错误。
		return nil
	}
	job.Status = model.BatchImageJobStatusSubmitted
	return nil
}

// pollOne 问一次上游进度并据此前推。
func (w *Worker) pollOne(ctx context.Context, job *model.BatchImageJob, provider Provider, channel ChannelContext) error {
	result, err := provider.Poll(ctx, PollRequest{
		Channel:   channel,
		BatchID:   job.BatchID,
		Model:     job.Model,
		JobName:   job.ProviderJobName,
		OutputRef: job.ProviderOutputRef,
	})
	if err != nil {
		var upstream *upstreamError
		if errors.As(err, &upstream) && upstream.Temporary() {
			return fmt.Errorf("temporary poll failure, will retry: %w", err)
		}
		return w.abort(ctx, job, "batch_image_poll_failed", err)
	}

	// 观测值先写下来（上游状态、结果对象地址），即使后面推进失败也能排查。
	progress := map[string]any{"provider_raw_state": result.RawState}
	if result.OutputRef != "" && result.OutputRef != job.ProviderOutputRef {
		progress["provider_output_ref"] = result.OutputRef
	}
	if err := op.BatchImageUpdateProgress(ctx, job.ID, progress); err != nil {
		log.Warnf("batch image worker: write progress for %s: %v", job.BatchID, err)
	}

	switch result.InternalState {
	case InternalStateSucceeded:
		// submitted → indexing。**先不要结账**：图还没数出来。
		advanced, err := op.BatchImageStatusCAS(ctx, job.ID,
			model.BatchImageJobStatusSubmitted, model.BatchImageJobStatusIndexing, map[string]any{
				"provider_output_ref": result.OutputRef,
			})
		if err != nil {
			return err
		}
		if advanced {
			job.Status = model.BatchImageJobStatusIndexing
			if result.OutputRef != "" {
				job.ProviderOutputRef = result.OutputRef
			}
		}
		return nil

	case InternalStateFailed, InternalStateCancelled, InternalStateExpired:
		code := result.ErrorCode
		if code == "" {
			code = "batch_image_upstream_" + string(result.InternalState)
		}
		message := result.ErrorMessage
		if message == "" {
			message = "the upstream job ended as " + string(result.InternalState)
		}
		return w.abort(ctx, job, code, errors.New(message))

	default:
		// queued / running：什么都不改，下一轮再看。
		return nil
	}
}

// indexOne 下载结果、逐行回填子项状态，然后结算。
func (w *Worker) indexOne(ctx context.Context, job *model.BatchImageJob, provider Provider, channel ChannelContext) error {
	reader, err := provider.OpenResult(ctx, ResultRequest{
		Channel:   channel,
		BatchID:   job.BatchID,
		Model:     job.Model,
		OutputRef: job.ProviderOutputRef,
	})
	if err != nil {
		var upstream *upstreamError
		if errors.As(err, &upstream) && upstream.Temporary() {
			return fmt.Errorf("temporary result open failure, will retry: %w", err)
		}
		return w.abort(ctx, job, "batch_image_result_unavailable", err)
	}
	defer reader.Close()

	items, err := op.BatchImageItemsByBatchID(ctx, job.BatchID)
	if err != nil {
		return err
	}
	byCustomID := make(map[string]*model.BatchImageItem, len(items))
	for index := range items {
		byCustomID[items[index].CustomID] = &items[index]
	}

	// 按 key 回填；key 缺失时用行号兜底（同一批次内行号与提交顺序一致）。
	//
	// 【为什么不只按行号】上游不保证结果顺序（官方 issue 里有乱序 bug）。
	// 只按行号回填会把 A 的图挂到 B 的名下，是静默的数据错位。
	var matched int
	parseErr := ParseResults(reader, func(lineNumber int, result ParsedResult) {
		var item *model.BatchImageItem
		if result.Key != "" {
			item = byCustomID[result.Key]
		} else if lineNumber-1 < len(items) {
			item = &items[lineNumber-1]
		}
		if item == nil {
			// 这一行的 key 对不上任何子项：上游多给了行。
			// 不能当成任何人的结果，丢掉并记一笔。
			log.Warnf("batch image worker: job %s result line %d has unknown key %q",
				job.BatchID, lineNumber, result.Key)
			return
		}

		fields := map[string]any{
			"provider_source_obj": job.ProviderOutputRef,
			"source_line_number":  lineNumber,
			"source_byte_offset":  0,
			"source_byte_length":  0,
			"indexed_at":          time.Now().Unix(),
		}
		if result.Error != nil {
			fields["status"] = model.BatchImageItemStatusFailed
			fields["error_code"] = result.Error.Code
			fields["error_message"] = truncateText(result.Error.Message, maxPublicErrorChars)
		} else if len(result.Images) > 0 {
			fields["status"] = model.BatchImageItemStatusSuccess
			fields["mime_type"] = result.Images[0].MimeType
			fields["file_extension"] = ImageExtension(result.Images[0].MimeType)
			fields["image_count"] = len(result.Images)
			matched++
		} else {
			fields["status"] = model.BatchImageItemStatusFailed
			fields["error_code"] = "no_image_returned"
			fields["error_message"] = "the upstream returned no image for this item"
		}

		if _, err := op.BatchImageItemIndexResult(ctx, item.ID, fields); err != nil {
			log.Errorf("batch image worker: index item %d of job %s: %v", item.ID, job.BatchID, err)
		}
	})
	if parseErr != nil {
		// 结果读到一半断了：任务保持 indexing，下一轮重来。
		// **不能标完成**——那样会把没数到的条目永久留在 pending，钱也按零成功结了。
		return fmt.Errorf("read results for job %s: %w", job.BatchID, parseErr)
	}

	// 上游没给结果的行：标失败并说明原因，别让它们永远 pending。
	pending, err := w.markMissingItemsFailed(ctx, job, items)
	if err != nil {
		return err
	}
	if pending > 0 {
		log.Warnf("batch image worker: job %s had %d items with no result line", job.BatchID, pending)
	}

	// 到这里才算真正完成。
	now := time.Now().Unix()
	advanced, err := op.BatchImageStatusCAS(ctx, job.ID,
		model.BatchImageJobStatusIndexing, model.BatchImageJobStatusCompleted, map[string]any{
			"completed_at": now,
		})
	if err != nil {
		return err
	}
	if !advanced {
		return nil
	}
	job.Status = model.BatchImageJobStatusCompleted

	// 结账。**必须走 Settle**，不能在这里自己算钱。
	if _, err := w.Service.Settle(ctx, job); err != nil {
		return fmt.Errorf("settle completed job %s: %w", job.BatchID, err)
	}

	// 清掉上游的输入文件（结果文件留着，客户端还要下）。
	w.cleanupUpstream(ctx, job, true, false)
	return nil
}

// markMissingItemsFailed 把还没有结果的子项标失败，返回标记了几条。
func (w *Worker) markMissingItemsFailed(ctx context.Context, job *model.BatchImageJob, items []model.BatchImageItem) (int, error) {
	marked := 0
	for index := range items {
		item := items[index]
		if item.Status != model.BatchImageItemStatusPending {
			continue
		}
		ok, err := op.BatchImageItemIndexResult(ctx, item.ID, map[string]any{
			"status":        model.BatchImageItemStatusFailed,
			"error_code":    "missing_result",
			"error_message": "the upstream returned no result line for this item",
		})
		if err != nil {
			return marked, err
		}
		if ok {
			marked++
		}
	}
	return marked, nil
}

// abort 把一条任务标失败、退款、清理。
func (w *Worker) abort(ctx context.Context, job *model.BatchImageJob, code string, cause error) error {
	message := "batch image job failed"
	if cause != nil {
		message = truncateText(sanitizeUpstreamMessage(cause.Error()), maxPublicErrorChars)
	}
	advanced, err := op.BatchImageStatusCAS(ctx, job.ID, job.Status,
		model.BatchImageJobStatusFailed, map[string]any{
			"error_code":    code,
			"error_message": message,
			"completed_at":  time.Now().Unix(),
		})
	if err != nil {
		return err
	}
	if !advanced {
		// 别人已经把它推走了（比如客户端刚取消）。不动钱。
		return nil
	}
	job.Status = model.BatchImageJobStatusFailed

	// 还在 pending 的子项一并标取消，免得客户端看到一堆永远 pending 的行。
	if _, err := op.BatchImageCancelPending(ctx, job.BatchID); err != nil {
		log.Warnf("batch image worker: cancel pending items of %s: %v", job.BatchID, err)
	}

	if _, err := w.Service.Settle(ctx, job); err != nil {
		log.Errorf("batch image worker: settle aborted job %s: %v", job.BatchID, err)
	}
	w.cleanupUpstream(ctx, job, true, false)
	return nil
}

// loadItems 从库里重建提交用的条目。
func (w *Worker) loadItems(ctx context.Context, job *model.BatchImageJob) ([]SubmitItem, error) {
	rows, err := op.BatchImageItemsByBatchID(ctx, job.BatchID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("batch image job %s has no items", job.BatchID)
	}
	items := make([]SubmitItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, SubmitItem{
			CustomID:    row.CustomID,
			Prompt:      row.Prompt,
			OutputCount: row.ImageCount,
		})
	}
	return items, nil
}

// resolve 找到这条任务该用的渠道与密钥。
func (w *Worker) resolve(ctx context.Context, job *model.BatchImageJob) (Provider, *model.Channel, *model.ChannelKey, error) {
	if w.Service.Resolver == nil {
		return nil, nil, nil, fmt.Errorf("batch image worker has no channel resolver")
	}
	channel, channelKey, err := w.Service.Resolver.Resolve(ctx, job.ChannelID, job.ChannelKeyID)
	if err != nil {
		return nil, nil, nil, err
	}
	if channel == nil {
		return nil, nil, nil, fmt.Errorf("channel %d not found", job.ChannelID)
	}
	providerFor := w.ProviderFor
	if providerFor == nil {
		providerFor = ProviderFor
	}
	provider, err := providerFor(channel)
	if err != nil {
		return nil, nil, nil, err
	}
	return provider, channel, channelKey, nil
}

// cancelUpstream 尽力让上游停下。
func (w *Worker) cancelUpstream(ctx context.Context, job *model.BatchImageJob) {
	if strings.TrimSpace(job.ProviderJobName) == "" {
		return
	}
	provider, channel, channelKey, err := w.resolve(ctx, job)
	if err != nil {
		return
	}
	if err := provider.Cancel(ctx, CancelRequest{
		Channel: channelContextOf(channel, channelKey),
		BatchID: job.BatchID,
		Model:   job.Model,
		JobName: job.ProviderJobName,
	}); err != nil {
		log.Warnf("batch image worker: cancel upstream job %s: %v", job.BatchID, err)
	}
}

// cleanupUpstream 尽力清理上游残留。
func (w *Worker) cleanupUpstream(ctx context.Context, job *model.BatchImageJob, deleteInput, deleteOutput bool) {
	if !deleteInput && !deleteOutput {
		return
	}
	provider, channel, channelKey, err := w.resolve(ctx, job)
	if err != nil {
		return
	}
	if err := provider.Cleanup(ctx, CleanupRequest{
		Channel:      channelContextOf(channel, channelKey),
		BatchID:      job.BatchID,
		InputRef:     job.ProviderInputRef,
		DeleteInput:  deleteInput,
		DeleteOutput: deleteOutput,
	}); err != nil {
		log.Warnf("batch image worker: cleanup upstream for %s: %v", job.BatchID, err)
	}
}

// channelContextOf 把渠道 + 密钥变成 provider 要的形状。
//
// 【密钥只在这里出现一次】它是唯一把 ChannelKey 原文交给 provider 的地方。
// 除了这里和 Resolver，别处都不该碰密钥原文。
func channelContextOf(channel *model.Channel, channelKey *model.ChannelKey) ChannelContext {
	out := ChannelContext{ChannelID: channel.ID}
	if channelKey != nil {
		out.ChannelKeyID = channelKey.ID
		// ChannelKey 在本仓里就是一个字符串密钥。对 gemini_api 它是 API key，
		// 对 Vertex 它是服务账号 JSON 原文——这个差别由渠道配置的 provider 决定，
		// 不由密钥栏的形状决定（密钥栏只有一列）。
		out.APIKey = channelKey.ChannelKey
	}
	out.BaseURL = channel.GetBaseUrl()
	if channel.BatchImage != nil {
		out.ProjectID = strings.TrimSpace(channel.BatchImage.ProjectID)
		out.Location = strings.TrimSpace(channel.BatchImage.Location)
		out.GCSBucket = strings.TrimSpace(channel.BatchImage.GCSBucket)
		out.GCSPrefix = strings.TrimSpace(channel.BatchImage.GCSPrefix)
	}
	// 自定义头照抄渠道配置（有些部署要靠它过网关）。
	if len(channel.CustomHeader) > 0 {
		out.Headers = make(map[string]string, len(channel.CustomHeader))
		for _, header := range channel.CustomHeader {
			if strings.TrimSpace(header.HeaderKey) == "" {
				continue
			}
			out.Headers[header.HeaderKey] = header.HeaderValue
		}
	}
	return out
}
