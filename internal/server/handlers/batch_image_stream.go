package handlers

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/batchimage"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// handler 与 batchimage 包之间的粘合：真正写字节的那几个函数在这里，
// 让 batch_image.go 只负责 HTTP 语义（状态码、头、错误映射）。

// batchImageContentResolver 是取图用的解析器（生产实现）。
//
// 做成变量而不是直接调包函数，是为了让 handler 全链路测试能塞一个假上游：
// 只测"返回了 404"不算验证过取图。
var batchImageContentResolver batchimage.ContentResolver = batchimage.DefaultContentResolver

// batchImageCancel 让上游停下并收口本地状态。
//
// 【顺序很重要】先让上游停，再改本地状态。反过来的话，我们这边已经标记"已取消"、
// 上游却还在跑——它会继续烧配额，而我们再也不会去 poll 它（状态是终态了），
// 等于永远不知道它其实跑完了。
func cancelBatchImageJob(ctx context.Context, job *model.BatchImageJob) (*model.BatchImageJob, error) {
	if job == nil {
		return nil, batchimage.ErrNotFound
	}
	if model.IsTerminalBatchImageJobStatus(job.Status) {
		// 已经是终态：取消是幂等的，直接把现状给他，不报错。
		// 报错会让客户端的"先取消再删除"流程卡在取消这一步。
		return job, nil
	}

	// 尽力撤上游。失败不阻断：上游可能已经自己跑完了，而本地状态必须收口，
	// 否则这条任务会一直挂在队列里被反复捡起来。
	if provider, channelCtx, err := resolveBatchImageProvider(ctx, job); err == nil {
		if job.ProviderJobName != "" {
			if err := provider.Cancel(ctx, batchimage.CancelRequest{
				Channel: channelCtx,
				BatchID: job.BatchID,
				Model:   job.Model,
				JobName: job.ProviderJobName,
			}); err != nil {
				log.Warnf("batch image: cancel upstream %s: %v", job.BatchID, err)
			}
		}
	} else {
		log.Warnf("batch image: cannot resolve provider to cancel %s: %v", job.BatchID, err)
	}

	advanced, err := op.BatchImageStatusCAS(ctx, job.ID, job.Status,
		model.BatchImageJobStatusCancelled, map[string]any{
			"completed_at": time.Now().Unix(),
		})
	if err != nil {
		return nil, err
	}
	if !advanced {
		// 别人抢先推走了（比如刚好跑完）。重新读一次，把最新状态给他。
		latest, err := op.BatchImageGetByBatchID(ctx, job.BatchID)
		if err != nil {
			return nil, err
		}
		if latest != nil {
			return latest, nil
		}
		return job, nil
	}
	job.Status = model.BatchImageJobStatusCancelled

	// 还在 pending 的子项标取消。
	if _, err := op.BatchImageCancelPending(ctx, job.BatchID); err != nil {
		log.Warnf("batch image: cancel pending items of %s: %v", job.BatchID, err)
	}

	// 结算：取消的条目不计费，所以这会把预扣的钱大部分退回去。
	if _, err := batchImageService.Settle(ctx, job); err != nil {
		log.Errorf("batch image: settle cancelled job %s: %v", job.BatchID, err)
	}
	return job, nil
}

// resolveBatchImageProvider 是取渠道 + provider 的薄封装。
func resolveBatchImageProvider(ctx context.Context, job *model.BatchImageJob) (batchimage.Provider, batchimage.ChannelContext, error) {
	if batchImageContentResolver == nil {
		return nil, batchimage.ChannelContext{}, fmt.Errorf("batch image: no content resolver")
	}
	return batchImageContentResolver(ctx, job)
}

// streamBatchImageItem 把单张图写进响应体。
func streamBatchImageItem(c *gin.Context, job *model.BatchImageJob, item *model.BatchImageItem) (bool, error) {
	reader, mimeType, err := batchimage.OpenItemImage(c.Request.Context(), batchImageContentResolver, job, item)
	if err != nil {
		if errors.Is(err, batchimage.ErrItemImageNotFound) {
			// 索引说成功但上游没有这一行：状态过期了（上游清理过结果）。
			// 不是 500——如实说取不到。
			return false, nil
		}
		return false, err
	}
	defer reader.Close()

	c.Header("Content-Type", mimeType)
	c.Header("Content-Disposition",
		fmt.Sprintf(`inline; filename="%s.%s"`, sanitizeFilename(item.CustomID), extensionForMime(mimeType)))
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, reader); err != nil {
		// 响应头已经发出了，没法再改状态码。记一笔让运维能看到。
		log.Warnf("batch image: copy item %s of %s: %v", item.CustomID, job.BatchID, err)
		return true, nil
	}

	// 下载时间是"这个用户真的来取图了"的唯一信号，记一下（失败不影响响应）。
	op.BatchImageMarkDownloaded(c.Request.Context(), job.ID)
	return true, nil
}

// streamBatchImageZip 把整批成功结果打成一个 zip 写到响应体。
//
// 【内存纪律】逐张写进 zip，写完就扔。不攒齐——一批 1000 张 4K 图是 GB 级的。
// zip.Writer 自己会做压缩缓冲，不需要我们再缓存。
func streamBatchImageZip(c *gin.Context, job *model.BatchImageJob, items []model.BatchImageItem) error {
	archive := zip.NewWriter(c.Writer)
	defer func() {
		if err := archive.Close(); err != nil {
			log.Warnf("batch image: close zip for %s: %v", job.BatchID, err)
		}
	}()

	written := 0
	err := batchimage.StreamAllImages(c.Request.Context(), batchImageContentResolver, job, items,
		func(image batchimage.ItemImage) error {
			// 客户端断开就别再读了。
			if err := c.Request.Context().Err(); err != nil {
				return err
			}
			name := fmt.Sprintf("%s.%s", sanitizeFilename(image.CustomID), extensionForMime(image.MimeType))
			entry, err := archive.CreateHeader(&zip.FileHeader{
				Name:     name,
				Method:   zip.Deflate,
				Modified: time.Now(),
			})
			if err != nil {
				return err
			}
			if _, err := entry.Write(image.Data); err != nil {
				return err
			}
			written++
			return nil
		})
	if err != nil {
		return err
	}

	// 附一份清单：用户拿到 zip 之后总得知道哪些成功了、哪些为什么没图。
	// 没有它，缺图就是个谜。
	manifest, err := buildBatchImageManifest(job, items)
	if err != nil {
		return err
	}
	entry, err := archive.CreateHeader(&zip.FileHeader{
		Name:     "manifest.json",
		Method:   zip.Deflate,
		Modified: time.Now(),
	})
	if err != nil {
		return err
	}
	if _, err := entry.Write(manifest); err != nil {
		return err
	}

	op.BatchImageMarkDownloaded(c.Request.Context(), job.ID)
	log.Debugf("batch image: streamed %d images for %s", written, job.BatchID)
	return nil
}

// extensionForMime 给出文件扩展名。
func extensionForMime(mimeType string) string {
	return batchimage.ImageExtension(mimeType)
}

// decodeBase64Image 解参考图的 base64。
func decodeBase64Image(data string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		// 也接受 URL-safe 与无填充变体：客户端从各种来源拼 base64 很常见，
		// 因为编码表不同就把整个提交拒掉属于白白添堵。
		decoded, err = base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("data_base64 is not valid base64")
		}
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("data_base64 decodes to zero bytes")
	}
	return decoded, nil
}

// batchImageErrorCodeFor 给 HTTP 层用的错误码（把内部错误翻成对外码）。
func batchImageErrorCodeFor(err error) string {
	code, _ := batchimage.PublicError(err)
	return code
}

// urlPathEscape 是 url.PathEscape 的别名（便于在 gin.H 里读得短一些）。
func urlPathEscape(value string) string {
	return url.PathEscape(value)
}

// buildBatchImageManifest 生成 zip 里的清单。
//
// 【为什么非要有】没有它，"我这批 200 张怎么只来了 187 张"就是个谜：
// 用户看不到哪 13 张失败了、更看不到为什么。有清单就能自己排查，
// 不用来问我们。
func buildBatchImageManifest(job *model.BatchImageJob, items []model.BatchImageItem) ([]byte, error) {
	type manifestItem struct {
		CustomID string `json:"custom_id"`
		Status   string `json:"status"`
		File     string `json:"file,omitempty"`
		Error    string `json:"error,omitempty"`
		Message  string `json:"message,omitempty"`
	}
	type manifest struct {
		BatchID     string         `json:"batch_id"`
		Model       string         `json:"model"`
		Status      string         `json:"status"`
		ItemCount   int            `json:"item_count"`
		Success     int            `json:"success_count"`
		Failed      int            `json:"fail_count"`
		Cancelled   int            `json:"cancelled_count"`
		GeneratedAt int64          `json:"generated_at"`
		Items       []manifestItem `json:"items"`
	}

	out := manifest{
		BatchID:     job.BatchID,
		Model:       job.RequestModel,
		Status:      job.Status,
		ItemCount:   job.ItemCount,
		Success:     job.SuccessCount,
		Failed:      job.FailCount,
		Cancelled:   job.CancelledCount,
		GeneratedAt: time.Now().Unix(),
		Items:       make([]manifestItem, 0, len(items)),
	}
	for _, item := range items {
		entry := manifestItem{
			CustomID: item.CustomID,
			Status:   item.Status,
			Error:    item.ErrorCode,
			Message:  item.ErrorMessage,
		}
		if item.Status == model.BatchImageItemStatusSuccess {
			entry.File = fmt.Sprintf("%s.%s", sanitizeFilename(item.CustomID), extensionForMime(item.MimeType))
		}
		out.Items = append(out.Items, entry)
	}
	return json.MarshalIndent(out, "", "  ")
}
