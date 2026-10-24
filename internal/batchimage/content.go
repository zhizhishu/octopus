package batchimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/zhizhishu/octopus/internal/model"
)

// 取图。
//
// 【为什么不把图片存下来】上游的结果文件本身就是唯一真源：它由上游托管、按自己的
// 保留期清理、多副本、可流式读。我们再存一份的代价是：一份 GB 级的存储、一套
// 一致性对账、还要回答"上游删了我这份留不留"这种没人想管的问题。
//
// 【所以取图的动作是】重新打开上游结果流，扫到对应的那一行，把那几张图的字节
// 直接转发给客户端。见 model.BatchImageItem 的注释：我们只存"它在上游的哪一行"。
//
// 【代价必须说清楚】每次取图都要重读结果流的前缀。一批 200 条、单条 4K 图的话，
// 取最后一条要读掉前面全部内容。这是刻意的取舍——本功能的使用方式是"跑完一次性
// 打包下载"，而不是"反复单张预览"。真要优化首图延迟，正确的做法是让上游提供按行
// 的 byte range，而不是我们自己存一份。

// ErrItemImageNotFound 表示结果流里没有这个子项的图。
var ErrItemImageNotFound = errors.New("batch image: no image for this item")

// ContentResolver 把一条任务解析成可用的 provider 与渠道。
//
// 抽成函数是为了让 handler 层不直接碰 ProviderFor / 密钥解析，
// 也让这条路径能在测试里用一个假上游跑通。
type ContentResolver func(ctx context.Context, job *model.BatchImageJob) (Provider, ChannelContext, error)

// DefaultContentResolver 用包级默认解析器（生产用）。
func DefaultContentResolver(ctx context.Context, job *model.BatchImageJob) (Provider, ChannelContext, error) {
	if job == nil {
		return nil, ChannelContext{}, fmt.Errorf("batch image: nil job")
	}
	if defaultResolver == nil {
		return nil, ChannelContext{}, fmt.Errorf("batch image: no channel resolver configured")
	}
	channel, channelKey, err := defaultResolver.Resolve(ctx, job.ChannelID, job.ChannelKeyID)
	if err != nil {
		return nil, ChannelContext{}, err
	}
	provider, err := ProviderFor(channel)
	if err != nil {
		return nil, ChannelContext{}, err
	}
	return provider, channelContextOf(channel, channelKey), nil
}

// OpenItemImage 打开某个子项的图片字节流。
//
// 返回的 ReadCloser 由调用方负责关闭。mimeType 取自结果里声明的类型。
func OpenItemImage(ctx context.Context, resolve ContentResolver, job *model.BatchImageJob, item *model.BatchImageItem) (io.ReadCloser, string, error) {
	if job == nil || item == nil {
		return nil, "", fmt.Errorf("batch image: open item image with nil job or item")
	}
	if resolve == nil {
		resolve = DefaultContentResolver
	}
	provider, channelCtx, err := resolve(ctx, job)
	if err != nil {
		return nil, "", err
	}

	reader, err := provider.OpenResult(ctx, ResultRequest{
		Channel:   channelCtx,
		BatchID:   job.BatchID,
		Model:     job.Model,
		OutputRef: job.ProviderOutputRef,
	})
	if err != nil {
		return nil, "", err
	}

	// 边读边找，找到就停。图片字节落在一个有界的单行里（见 maxResultLineBytes）。
	var (
		image    ResultImage
		mimeType string
		found    bool
	)
	parseErr := ParseResults(reader, func(_ int, result ParsedResult) {
		if found {
			return
		}
		if result.Key != "" && result.Key != item.CustomID {
			return
		}
		if len(result.Images) == 0 {
			return
		}
		image = result.Images[0]
		mimeType = image.MimeType
		found = true
	})
	// 关掉上游流：图已经在内存里了，继续持有连接没有意义。
	_ = reader.Close()
	if parseErr != nil && !found {
		return nil, "", parseErr
	}
	if !found {
		return nil, "", ErrItemImageNotFound
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return io.NopCloser(bytes.NewReader(image.Data)), mimeType, nil
}

// ItemImage 是打包下载时的一项。
type ItemImage struct {
	CustomID string
	MimeType string
	Data     []byte
}

// StreamAllImages 依次把每个成功子项的第一张图交给 onImage。
//
// 【为什么用回调而不是返回切片】一批 1000 张 4K 图是 GB 级的。
// 攒齐再交给调用方等于把整批读进内存；回调让调用方可以逐张写进 zip 就扔掉。
//
// 某个子项没有图不算错误，那个子项会被跳过（调用方从 onImage 未被调用即可知道）。
// onImage 返回的错误会**立即中断**整个流并原样返回——客户端断开时必须马上停，
// 否则我们会把剩下几百 MB 从上游一路读进内存再丢弃。
func StreamAllImages(ctx context.Context, resolve ContentResolver, job *model.BatchImageJob, items []model.BatchImageItem, onImage func(ItemImage) error) error {
	if job == nil {
		return fmt.Errorf("batch image: stream images with nil job")
	}
	if resolve == nil {
		resolve = DefaultContentResolver
	}
	if len(items) == 0 {
		return nil
	}

	// wanted = 需要出图的（成功的那些）；order = **全部**条目按提交顺序。
	//
	// 【为什么 order 必须是全部而不是只成功的】ParseResults 给的 lineNumber 是
	// 结果流里的**物理行号**，而结果流是按提交顺序一行一条（含失败行）。若 order
	// 只装成功的条目，第 5 行（成功）会被映射成 order[4]，也就是"第 5 个成功的条目"
	// 而不是"第 5 个条目"—— 一旦前面有失败行，图就挂到别人名下了。
	// worker.go 的 indexOne 用的是全量 items，这里必须同口径。
	wanted := make(map[string]model.BatchImageItem, len(items))
	order := make([]string, 0, len(items))
	for _, item := range items {
		order = append(order, item.CustomID)
		if item.Status != model.BatchImageItemStatusSuccess {
			continue
		}
		wanted[item.CustomID] = item
	}
	if len(wanted) == 0 {
		return nil
	}

	provider, channelCtx, err := resolve(ctx, job)
	if err != nil {
		return err
	}
	reader, err := provider.OpenResult(ctx, ResultRequest{
		Channel:   channelCtx,
		BatchID:   job.BatchID,
		Model:     job.Model,
		OutputRef: job.ProviderOutputRef,
	})
	if err != nil {
		return err
	}
	defer reader.Close()

	// 回调错误用**局部**变量捕获（不能是包级变量：并发打包的多个请求会互相覆盖，
	// 那会让 A 的客户端断开导致 B 的响应被判定为失败）。
	var sendErr error
	emitted := make(map[string]bool, len(wanted))

	// 上游可能乱序（见 worker.go 里 indexOne 的注释），所以按 key 匹配而不是按出现顺序。
	parseErr := ParseResults(reader, func(lineNumber int, result ParsedResult) {
		if sendErr != nil {
			return
		}
		if len(result.Images) == 0 {
			return
		}
		customID := result.Key
		if customID == "" && lineNumber-1 < len(order) {
			// 没有 key 时用行号兜底（与 worker 的索引口径一致）。
			customID = order[lineNumber-1]
		}
		item, ok := wanted[customID]
		if !ok || emitted[customID] {
			return
		}
		emitted[customID] = true
		mimeType := result.Images[0].MimeType
		if mimeType == "" {
			mimeType = "image/png"
		}
		sendErr = onImage(ItemImage{
			CustomID: item.CustomID,
			MimeType: mimeType,
			Data:     result.Images[0].Data,
		})
	})

	// 回调的错优先：它说明客户端已经走了，此时上游的读取错误没有意义。
	if sendErr != nil {
		return sendErr
	}
	return parseErr
}
