package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/cache"
	"gorm.io/gorm"
)

var llmModelCache = cache.New[string, model.LLMPrice](16)

// llmSourceCache mirrors LLMInfo.Source next to llmModelCache. The price cache stays a
// plain LLMPrice map because billing reads it on every request.
var llmSourceCache = cache.New[string, string](16)

// llmWriteMu serialises every price-row write that reads before it writes (edit,
// library apply, source classification), so a library apply's "is the row still what
// the preview showed" check cannot race an edit.
var llmWriteMu sync.Mutex

// ErrLLMPriceConflict means a price row no longer matches what the caller previewed.
// Handlers map it to HTTP 409 so the page reloads the preview instead of overwriting.
var ErrLLMPriceConflict = errors.New("model price changed since the preview; reload and try again")

func normalizeLLMName(name string) string {
	return strings.ToLower(model.CleanOneMillionCapabilityModelName(name))
}

func LLMList(ctx context.Context) ([]model.LLMInfo, error) {
	merged := make(map[string]model.LLMPrice, llmModelCache.Len())
	for m, cost := range llmModelCache.GetAll() {
		name := model.CleanOneMillionCapabilityModelName(m)
		if name == "" {
			continue
		}
		merged[name] = cost
	}
	models := make([]model.LLMInfo, 0, len(merged))
	for name, cost := range merged {
		source, _ := llmSourceCache.Get(name)
		models = append(models, model.LLMInfo{Name: name, LLMPrice: cost, Source: source})
	}
	return models, nil
}

func LLMUpdate(info model.LLMInfo, ctx context.Context) error {
	llmWriteMu.Lock()
	defer llmWriteMu.Unlock()
	return llmUpdateLocked(info, ctx)
}

// LLMUpdateWith reads the stored row, lets fn build the new row from it, and saves the
// result, all under the price write lock. updateLLM uses it so the manual-source marking
// is decided against the row it actually overwrites.
func LLMUpdateWith(name string, ctx context.Context, fn func(stored model.LLMInfo) (model.LLMInfo, error)) (model.LLMInfo, error) {
	llmWriteMu.Lock()
	defer llmWriteMu.Unlock()
	name = normalizeLLMName(name)
	price, ok := llmModelCache.Get(name)
	if !ok {
		return model.LLMInfo{}, fmt.Errorf("model not found")
	}
	source, _ := llmSourceCache.Get(name)
	next, err := fn(model.LLMInfo{Name: name, LLMPrice: price, Source: source})
	if err != nil {
		return model.LLMInfo{}, err
	}
	next.Name = name
	if err := llmUpdateLocked(next, ctx); err != nil {
		return model.LLMInfo{}, err
	}
	return next, nil
}

func llmUpdateLocked(info model.LLMInfo, ctx context.Context) error {
	info.Name = normalizeLLMName(info.Name)
	_, ok := llmModelCache.Get(info.Name)
	if !ok {
		return fmt.Errorf("model not found")
	}
	if err := db.GetDB().WithContext(ctx).Save(info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, info.LLMPrice)
	llmSourceCache.Set(info.Name, info.Source)
	return nil
}

func LLMDelete(modelName string, ctx context.Context) error {
	modelName = strings.ToLower(model.CleanOneMillionCapabilityModelName(modelName))
	_, ok := llmModelCache.Get(modelName)
	if !ok {
		return fmt.Errorf("model not found")
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.LLMInfo{Name: modelName}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelName)
	llmSourceCache.Del(modelName)
	return nil
}
func LLMBatchDelete(modelNames []string, ctx context.Context) error {
	if len(modelNames) == 0 {
		return nil
	}
	modelNames = model.NormalizeChannelModelNames(modelNames)
	for i := range modelNames {
		modelNames[i] = strings.ToLower(modelNames[i])
	}
	if err := db.GetDB().WithContext(ctx).Where("name IN ?", modelNames).Delete(&model.LLMInfo{}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelNames...)
	llmSourceCache.Del(modelNames...)
	return nil
}
func LLMCreate(info model.LLMInfo, ctx context.Context) error {
	info.Name = strings.ToLower(model.CleanOneMillionCapabilityModelName(info.Name))
	_, ok := llmModelCache.Get(info.Name)
	if ok {
		return fmt.Errorf("model already exists")
	}
	if err := db.GetDB().WithContext(ctx).Create(&info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, info.LLMPrice)
	llmSourceCache.Set(info.Name, info.Source)
	return nil
}
func LLMBatchCreate(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(llmInfos))
	newLLMInfos := make([]model.LLMInfo, 0, len(llmInfos))
	for _, llmInfo := range llmInfos {
		llmInfo.Name = strings.ToLower(model.CleanOneMillionCapabilityModelName(llmInfo.Name))
		if _, ok := seen[llmInfo.Name]; ok {
			continue
		}
		if _, ok := llmModelCache.Get(llmInfo.Name); ok {
			continue
		}
		seen[llmInfo.Name] = struct{}{}
		newLLMInfos = append(newLLMInfos, llmInfo)
	}
	if len(newLLMInfos) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Create(&newLLMInfos).Error; err != nil {
		return err
	}
	for _, llmInfo := range newLLMInfos {
		llmModelCache.Set(llmInfo.Name, llmInfo.LLMPrice)
		llmSourceCache.Set(llmInfo.Name, llmInfo.Source)
	}
	return nil
}
func LLMGet(name string) (model.LLMPrice, error) {
	name = strings.ToLower(model.CleanOneMillionCapabilityModelName(name))
	price, ok := llmModelCache.Get(name)
	if !ok {
		return model.LLMPrice{}, fmt.Errorf("model not found")
	}
	return price, nil
}

func llmRefreshCache(ctx context.Context) error {
	models := []model.LLMInfo{}
	if err := db.GetDB().WithContext(ctx).Find(&models).Error; err != nil {
		return err
	}
	modelMap := make(map[string]model.LLMPrice, len(models))
	sourceMap := make(map[string]string, len(models))
	for _, info := range models {
		name := strings.ToLower(model.CleanOneMillionCapabilityModelName(info.Name))
		if name == "" {
			continue
		}
		modelMap[name] = info.LLMPrice
		sourceMap[name] = info.Source
	}
	llmModelCache.ReplaceAll(modelMap)
	llmSourceCache.ReplaceAll(sourceMap)
	return nil
}

// LLMGetInfo returns the cached row (price and source) for a model.
func LLMGetInfo(name string) (model.LLMInfo, bool) {
	name = normalizeLLMName(name)
	price, ok := llmModelCache.Get(name)
	if !ok {
		return model.LLMInfo{}, false
	}
	source, _ := llmSourceCache.Get(name)
	return model.LLMInfo{Name: name, LLMPrice: price, Source: source}, true
}

// LLMClassifySources gives every legacy row (empty source) a source. classify gets the
// stored row and returns LLMSourceLibrary or LLMSourceManual. Rows that already have a
// source are never reclassified, so an admin's manual marking survives restarts.
func LLMClassifySources(ctx context.Context, classify func(info model.LLMInfo) string) (int, error) {
	llmWriteMu.Lock()
	defer llmWriteMu.Unlock()
	var rows []model.LLMInfo
	if err := db.GetDB().WithContext(ctx).Where("source = ? OR source IS NULL", "").Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			rows[i].Source = classify(rows[i])
			if err := tx.Model(&model.LLMInfo{}).Where("name = ?", rows[i].Name).Update("source", rows[i].Source).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		llmSourceCache.Set(normalizeLLMName(row.Name), row.Source)
	}
	return len(rows), nil
}

// LLMLibraryChange writes one model's library token price. Expected* is what the
// caller saw when it decided on the change; Next is the library price to write.
type LLMLibraryChange struct {
	Name           string
	ExpectedPrice  model.LLMPrice
	ExpectedSource string
	// ExpectedExists is false when the preview saw no row at all (a channel model
	// whose row the model sync has not created yet).
	ExpectedExists bool
	Next           model.LLMPrice
}

// LLMApplyLibraryPrices writes library token prices in one transaction. Every row must
// still match what the caller previewed (token price, source, existence); otherwise
// nothing is written and ErrLLMPriceConflict is returned. PerCall is always kept from
// the stored row: the library has no per-call prices. Written rows become library rows.
func LLMApplyLibraryPrices(ctx context.Context, changes []LLMLibraryChange) error {
	if len(changes) == 0 {
		return nil
	}
	llmWriteMu.Lock()
	defer llmWriteMu.Unlock()
	written := make([]model.LLMInfo, 0, len(changes))
	seen := make(map[string]struct{}, len(changes))
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, change := range changes {
			name := normalizeLLMName(change.Name)
			if name == "" {
				return fmt.Errorf("empty model name")
			}
			if _, dup := seen[name]; dup {
				return fmt.Errorf("duplicate model %s", name)
			}
			seen[name] = struct{}{}
			var rows []model.LLMInfo
			if err := tx.Where("name = ?", name).Limit(1).Find(&rows).Error; err != nil {
				return err
			}
			exists := len(rows) == 1
			if exists != change.ExpectedExists {
				return fmt.Errorf("%w: %s", ErrLLMPriceConflict, name)
			}
			row := model.LLMInfo{Name: name}
			if exists {
				row = rows[0]
				if !row.LLMPrice.SameTokenPrice(change.ExpectedPrice) || row.Source != change.ExpectedSource {
					return fmt.Errorf("%w: %s", ErrLLMPriceConflict, name)
				}
			}
			row.Name = name
			row.Input = change.Next.Input
			row.Output = change.Next.Output
			row.CacheRead = change.Next.CacheRead
			row.CacheWrite = change.Next.CacheWrite
			row.Source = model.LLMSourceLibrary
			var err error
			if exists {
				err = tx.Save(&row).Error
			} else {
				err = tx.Create(&row).Error
			}
			if err != nil {
				return err
			}
			written = append(written, row)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, row := range written {
		llmModelCache.Set(row.Name, row.LLMPrice)
		llmSourceCache.Set(row.Name, row.Source)
	}
	return nil
}

// PriceSyncRunAdd records one sync run and keeps only the newest keep runs.
func PriceSyncRunAdd(ctx context.Context, run model.PriceSyncRun, keep int) error {
	conn := db.GetDB().WithContext(ctx)
	if err := conn.Create(&run).Error; err != nil {
		return err
	}
	if keep <= 0 {
		return nil
	}
	var stale []uint
	if err := conn.Model(&model.PriceSyncRun{}).Order("id DESC").Offset(keep).Pluck("id", &stale).Error; err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	return conn.Where("id IN ?", stale).Delete(&model.PriceSyncRun{}).Error
}

// PriceSyncRunList returns the newest runs first.
func PriceSyncRunList(ctx context.Context, limit int) ([]model.PriceSyncRun, error) {
	var runs []model.PriceSyncRun
	q := db.GetDB().WithContext(ctx).Order("id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}
