package batchimage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// Configured users create the batch, children, receipt and precharge in one
// transaction. The unconfigured branch remains in Service.Submit unchanged.
func (s *Service) submitWithUserQuota(ctx context.Context, input SubmitInput, settings Settings, selection *Selection) (*model.BatchImageJob, bool, error) {
	enabled, err := op.UserQuotaEnabled(ctx, input.OwnerUserID)
	if err != nil || !enabled {
		return nil, false, err
	}
	if err := validateBuildable(input); err != nil {
		return nil, true, err
	}
	hold := settings.UnitPrice * float64(countOutputs(input.Items))
	if hold > 0 {
		enough, err := op.UserHasBalance(input.OwnerUserID, hold, ctx)
		if err != nil {
			return nil, true, err
		}
		if !enough {
			return nil, true, fmt.Errorf("%w: need %.4f", ErrInsufficientQuota, hold)
		}
	}
	requestJSON, err := json.Marshal(map[string]any{
		"model": input.Model, "response_mime_type": input.ResponseMimeType,
		"aspect_ratio": input.AspectRatio, "image_size": input.ImageSize,
		"provider": selection.Provider, "items": redactItemsForStorage(input.Items),
	})
	if err != nil {
		return nil, true, fmt.Errorf("batch image: encode request: %w", err)
	}
	now := time.Now()
	job := &model.BatchImageJob{
		BatchID: op.BatchImageNewID(), Status: model.BatchImageJobStatusQueued,
		Model: selection.UpstreamModel, RequestModel: input.Model,
		ChannelID: selection.Channel.ID, ChannelKeyID: selection.ChannelKey.ID,
		Provider: selection.Provider, OwnerUserID: input.OwnerUserID, OwnerTokenID: input.OwnerTokenID,
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey), RequestHash: s.requestHash(input),
		ItemCount: len(input.Items), ResponseMimeType: input.ResponseMimeType,
		AspectRatio: input.AspectRatio, ImageSize: input.ImageSize,
		RequestJSON: string(requestJSON), HoldAmount: hold, UnitPriceSnapshot: settings.UnitPrice,
		ExpiresAt: now.Add(time.Duration(settings.RetentionHours) * time.Hour).Unix(), Metadata: input.Metadata,
	}
	items := make([]model.BatchImageItem, 0, len(input.Items))
	for index, item := range input.Items {
		referenceJSON, err := json.Marshal(item.ReferenceImages)
		if err != nil {
			return nil, true, fmt.Errorf("batch image: encode reference images: %w", err)
		}
		items = append(items, model.BatchImageItem{
			BatchID: job.BatchID, CustomID: strings.TrimSpace(item.CustomID),
			Status: model.BatchImageItemStatusPending, PromptPreview: previewOf(item.Prompt),
			Prompt: item.Prompt, ReferenceImagesJSON: string(referenceJSON),
			RequestHash: op.BatchImageRequestHash([]byte(item.Prompt)), SourceLineNumber: index + 1,
			ProviderSourceObj: "", ImageCount: item.OutputCount,
		})
	}
	handled, err := op.UserQuotaBatchCreate(ctx, job, items)
	if err != nil && job.IdempotencyKey != "" {
		existing, lookupErr := op.BatchImageGetByIdempotencyKey(ctx, input.OwnerUserID, input.OwnerTokenID, job.IdempotencyKey)
		if lookupErr != nil {
			return nil, true, lookupErr
		}
		if existing != nil {
			if existing.RequestHash != job.RequestHash {
				return nil, true, ErrIdempotencyConflict
			}
			return existing, true, nil
		}
	}
	if err != nil {
		return nil, true, err
	}
	if !handled {
		return nil, false, nil
	} // disabled concurrently before admission
	return job, true, nil
}

func (s *Service) settleWithUserQuota(ctx context.Context, job *model.BatchImageJob) (handled, settled bool, err error) {
	exists, err := op.UserQuotaReceiptExists(ctx, "batch:"+job.BatchID, job.OwnerUserID)
	if err != nil || !exists {
		return false, false, err
	}
	success, failed, cancelled, err := op.BatchImageCountItems(ctx, job.BatchID)
	if err != nil {
		return true, false, err
	}
	unitPrice := job.UnitPriceSnapshot
	if unitPrice <= 0 {
		unitPrice = s.settings(ctx).UnitPrice
	}
	charged := unitPrice * float64(success)
	if charged > job.HoldAmount {
		charged = job.HoldAmount
	}
	return op.UserQuotaBatchSettle(ctx, job, charged, success, failed, cancelled)
}
