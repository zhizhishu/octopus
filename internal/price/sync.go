package price

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// Preview kinds. They match the tabs of the "从上游更新价格" dialog.
const (
	SyncKindNew     = "new"     // the row has no token price yet and the library has one
	SyncKindChanged = "changed" // a library row whose price differs from the library
	SyncKindManual  = "manual"  // a manual row whose price differs from the library
	SyncKindSame    = "same"    // already equal to the library
)

// BigChangeRatio: a change of more than ±50% on any token rate that was already set is a
// "big change". The preview leaves it unticked and the auto-sync holds it back.
const BigChangeRatio = 0.5

// priceSyncRunKeep is how many runs the history keeps.
const priceSyncRunKeep = 20

// builtinPrice is the catalog shipped in presets.go, kept before any fetch merges into
// it. Legacy rows equal to it were created from the library, not typed by an admin.
var builtinPrice = clonePrices(llmPrice)

func clonePrices(in map[string]model.LLMPrice) map[string]model.LLMPrice {
	out := make(map[string]model.LLMPrice, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

var (
	syncMu       sync.Mutex
	lastAutoRun  time.Time
	lastAutoRunM sync.RWMutex
)

// SyncPrice is the four token rates shown in the preview.
type SyncPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func toSyncPrice(p model.LLMPrice) SyncPrice {
	return SyncPrice{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
}

func (p SyncPrice) llm() model.LLMPrice {
	return model.LLMPrice{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
}

// SyncItem is one model in the preview.
type SyncItem struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Source string `json:"source"` // stored source; "" when the row does not exist yet
	Exists bool   `json:"exists"`
	// Current is the stored token price (what apply expects to find), Next the library price.
	Current SyncPrice `json:"current"`
	Next    SyncPrice `json:"next"`
	Big     bool      `json:"big"`
	// HasPerCall tells the page this model also has a per-call price that sync keeps.
	HasPerCall bool `json:"has_per_call"`
}

// SyncPreview is the whole preview: only models selected in your channels.
type SyncPreview struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Total     int        `json:"total"`      // channel models looked at
	NoLibrary int        `json:"no_library"` // channel models the library has no price for
	Items     []SyncItem `json:"items"`
}

// SyncApplyChange is one ticked row sent back from the preview.
type SyncApplyChange struct {
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	Exists  bool      `json:"exists"`
	Current SyncPrice `json:"current"`
	Next    SyncPrice `json:"next"`
}

// SyncApplyResult counts what an apply wrote.
type SyncApplyResult struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
}

// IsBigChange reports whether any rate that was already set moves by more than ±50%.
// A rate going from 0 to something ("原来没填") is not a big change.
func IsBigChange(from, to model.LLMPrice) bool {
	pairs := [][2]float64{{from.Input, to.Input}, {from.Output, to.Output}, {from.CacheRead, to.CacheRead}, {from.CacheWrite, to.CacheWrite}}
	for _, p := range pairs {
		if p[0] > 0 && math.Abs(p[1]-p[0])/p[0] > BigChangeRatio+1e-9 {
			return true
		}
	}
	return false
}

func lookupLibrary(lib map[string]model.LLMPrice, name string) (model.LLMPrice, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if p, ok := lib[name]; ok && !p.TokenPriceIsZero() {
		return p, true
	}
	if i := strings.LastIndexAny(name, "/:"); i >= 0 && i+1 < len(name) {
		if p, ok := lib[name[i+1:]]; ok && !p.TokenPriceIsZero() {
			return p, true
		}
	}
	return model.LLMPrice{}, false
}

// ClassifyLegacySources gives legacy rows (no source yet) a source: zero, or equal to
// the freshly fetched library or the built-in snapshot, becomes library; anything else
// was typed by someone and becomes manual.
func ClassifyLegacySources(ctx context.Context, lib map[string]model.LLMPrice) (int, error) {
	return op.LLMClassifySources(ctx, func(info model.LLMInfo) string {
		if info.LLMPrice.TokenPriceIsZero() {
			return model.LLMSourceLibrary
		}
		if p, ok := lookupLibrary(lib, info.Name); ok && p.SameTokenPrice(info.LLMPrice) {
			return model.LLMSourceLibrary
		}
		if p, ok := lookupLibrary(builtinPrice, info.Name); ok && p.SameTokenPrice(info.LLMPrice) {
			return model.LLMSourceLibrary
		}
		return model.LLMSourceManual
	})
}

func channelModelNames(ctx context.Context) ([]string, error) {
	channels, err := op.ChannelList(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	names := make([]string, 0, 64)
	for _, ch := range channels {
		for _, m := range model.ChannelSelectedModelNames(ch) {
			n := strings.ToLower(model.CleanOneMillionCapabilityModelName(strings.TrimSpace(m)))
			if n == "" {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			names = append(names, n)
		}
	}
	return names, nil
}

// buildPreview compares the stored rows of every channel model with a library view.
func buildPreview(ctx context.Context, library func(string) (model.LLMPrice, bool), fetchedAt time.Time) (SyncPreview, error) {
	names, err := channelModelNames(ctx)
	if err != nil {
		return SyncPreview{}, err
	}
	out := SyncPreview{FetchedAt: fetchedAt, Total: len(names), Items: make([]SyncItem, 0, len(names))}
	for _, name := range names {
		next, ok := library(name)
		if !ok {
			out.NoLibrary++
			continue
		}
		info, exists := op.LLMGetInfo(name)
		item := SyncItem{
			Name:       name,
			Source:     info.Source,
			Exists:     exists,
			Current:    toSyncPrice(info.LLMPrice),
			Next:       toSyncPrice(next),
			HasPerCall: info.PerCall > 0,
		}
		same := info.LLMPrice.SameTokenPrice(next)
		switch {
		case same:
			item.Kind = SyncKindSame
		case exists && info.Source == model.LLMSourceManual:
			item.Kind = SyncKindManual
		case info.LLMPrice.TokenPriceIsZero():
			item.Kind = SyncKindNew
		default:
			item.Kind = SyncKindChanged
		}
		if item.Kind == SyncKindChanged || item.Kind == SyncKindManual {
			item.Big = IsBigChange(info.LLMPrice, next)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Preview fetches models.dev into a separate snapshot, classifies legacy rows, and
// returns the diff for the models in your channels. It is read-only for billing: it
// writes no price row and leaves the in-memory catalog alone ("你勾了才改"). Only
// Apply (for the ticked rows) and the auto-sync run change what is billed.
func Preview(ctx context.Context) (SyncPreview, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	lib, at, err := FetchSnapshot(ctx)
	if err != nil {
		return SyncPreview{}, err
	}
	if _, err := ClassifyLegacySources(ctx, lib); err != nil {
		return SyncPreview{}, err
	}
	return buildPreview(ctx, func(name string) (model.LLMPrice, bool) { return libraryViewPrice(lib, name) }, at)
}

// Apply writes the ticked preview rows. Every row must still look the way the preview
// showed it and the library must still have the same price; otherwise nothing is
// written and an error wrapping op.ErrLLMPriceConflict is returned (HTTP 409).
func Apply(ctx context.Context, changes []SyncApplyChange) (SyncApplyResult, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	res, err := applyLocked(ctx, changes, LatestLibraryPrice)
	if err != nil {
		return res, err
	}
	if res.Added+res.Changed > 0 {
		recordRun(ctx, model.PriceSyncRun{Trigger: model.PriceSyncTriggerManual, Status: model.PriceSyncStatusOK, Added: res.Added, Changed: res.Changed})
	}
	return res, nil
}

// applyLocked writes the ticked rows, checked against the library view, then puts
// their library prices into the billing catalog (only those names). Callers hold syncMu.
func applyLocked(ctx context.Context, changes []SyncApplyChange, library func(string) (model.LLMPrice, bool)) (SyncApplyResult, error) {
	var res SyncApplyResult
	ops := make([]op.LLMLibraryChange, 0, len(changes))
	applied := make(map[string]model.LLMPrice, len(changes))
	for _, c := range changes {
		lib, ok := library(c.Name)
		if !ok || !lib.SameTokenPrice(c.Next.llm()) {
			return SyncApplyResult{}, errors.Join(op.ErrLLMPriceConflict, errors.New("the library price changed; reload the preview"))
		}
		ops = append(ops, op.LLMLibraryChange{
			Name:           c.Name,
			ExpectedPrice:  c.Current.llm(),
			ExpectedSource: c.Source,
			ExpectedExists: c.Exists,
			Next:           lib,
		})
		applied[strings.ToLower(strings.TrimSpace(c.Name))] = lib
		if !c.Exists || c.Current.llm().TokenPriceIsZero() {
			res.Added++
		} else {
			res.Changed++
		}
	}
	if err := op.LLMApplyLibraryPrices(ctx, ops); err != nil {
		return SyncApplyResult{}, err
	}
	fetchMu.Lock()
	mergeCatalog(applied)
	fetchMu.Unlock()
	return res, nil
}

// RestoreLibraryPrice puts a row back on the library price and makes it follow the
// library again ("恢复上游价"). PerCall is kept.
func RestoreLibraryPrice(ctx context.Context, name string) (model.LLMInfo, error) {
	lib, ok := CatalogPrice(name)
	if !ok {
		return model.LLMInfo{}, ErrNoLibraryPrice
	}
	return op.LLMUpdateWith(name, ctx, func(stored model.LLMInfo) (model.LLMInfo, error) {
		stored.Input, stored.Output, stored.CacheRead, stored.CacheWrite = lib.Input, lib.Output, lib.CacheRead, lib.CacheWrite
		stored.Source = model.LLMSourceLibrary
		return stored, nil
	})
}

// ErrNoLibraryPrice: the library has no price for the model, so there is nothing to restore.
var ErrNoLibraryPrice = errors.New("the price library has no price for this model")

func settingBoolOr(key model.SettingKey, def bool) bool {
	v, err := op.SettingGetBool(key)
	if err != nil {
		return def
	}
	return v
}

func recordRun(ctx context.Context, run model.PriceSyncRun) {
	if run.At.IsZero() {
		run.At = time.Now()
	}
	if err := op.PriceSyncRunAdd(ctx, run, priceSyncRunKeep); err != nil {
		log.Warnf("price sync: failed to record run: %v", err)
	}
}

// AutoSync is the periodic task (every model_info_update_interval hours). It always
// refreshes the catalog. When price_auto_sync_enabled is on it then writes only
// library rows that are new or changed; manual rows are never touched; with
// price_sync_hold_big_change on, changes over ±50% are held back for the admin. A
// failed or abnormally small fetch changes nothing.
func AutoSync(ctx context.Context) model.PriceSyncRun {
	syncMu.Lock()
	defer syncMu.Unlock()
	enabled := settingBoolOr(model.SettingKeyPriceAutoSyncEnabled, true)
	if enabled {
		lastAutoRunM.Lock()
		lastAutoRun = time.Now()
		lastAutoRunM.Unlock()
	}
	run := model.PriceSyncRun{Trigger: model.PriceSyncTriggerAuto, Status: model.PriceSyncStatusOK}
	lib, err := FetchLibrary(ctx)
	if err != nil {
		run.Status = model.PriceSyncStatusFailed
		if errors.Is(err, ErrLibraryTooSmall) {
			run.Status = model.PriceSyncStatusAborted
		}
		run.Message = err.Error()
		log.Warnf("price auto sync: %v", err)
		if enabled {
			recordRun(ctx, run)
		}
		return run
	}
	if _, err := ClassifyLegacySources(ctx, lib); err != nil {
		run.Status = model.PriceSyncStatusFailed
		run.Message = err.Error()
		if enabled {
			recordRun(ctx, run)
		}
		return run
	}
	if !enabled {
		return run
	}
	preview, err := buildPreview(ctx, CatalogPrice, GetLastUpdateTime())
	if err != nil {
		run.Status = model.PriceSyncStatusFailed
		run.Message = err.Error()
		recordRun(ctx, run)
		return run
	}
	hold := settingBoolOr(model.SettingKeyPriceSyncHoldBigChange, true)
	changes := make([]SyncApplyChange, 0)
	held := make([]string, 0)
	for _, it := range preview.Items {
		if it.Kind != SyncKindNew && it.Kind != SyncKindChanged {
			continue
		}
		if it.Exists && it.Source == model.LLMSourceManual {
			continue // never: buildPreview already files these as manual
		}
		if hold && it.Big {
			held = append(held, it.Name)
			continue
		}
		changes = append(changes, SyncApplyChange{Name: it.Name, Source: it.Source, Exists: it.Exists, Current: it.Current, Next: it.Next})
	}
	res, err := applyLocked(ctx, changes, CatalogPrice)
	if err != nil {
		run.Status = model.PriceSyncStatusFailed
		if errors.Is(err, op.ErrLLMPriceConflict) {
			run.Status = model.PriceSyncStatusConflict
		}
		run.Message = err.Error()
		recordRun(ctx, run)
		return run
	}
	run.Added, run.Changed = res.Added, res.Changed
	run.Held = len(held)
	run.HeldModels = strings.Join(held, "\n")
	recordRun(ctx, run)
	return run
}

// SyncStatus feeds the price page header and the auto-update dialog.
type SyncStatus struct {
	AutoEnabled   bool                 `json:"auto_enabled"`
	HoldBigChange bool                 `json:"hold_big_change"`
	IntervalHours int                  `json:"interval_hours"`
	LastFetchAt   time.Time            `json:"last_fetch_at"`
	NextRunAt     *time.Time           `json:"next_run_at,omitempty"`
	Runs          []model.PriceSyncRun `json:"runs"`
	// HeldPending are models the newest auto run held back that still differ from the
	// library by a big change and still follow it. The page shows a banner for them.
	HeldPending []string `json:"held_pending"`
}

func Status(ctx context.Context) (SyncStatus, error) {
	st := SyncStatus{
		AutoEnabled:   settingBoolOr(model.SettingKeyPriceAutoSyncEnabled, true),
		HoldBigChange: settingBoolOr(model.SettingKeyPriceSyncHoldBigChange, true),
		LastFetchAt:   GetLastUpdateTime(),
		HeldPending:   []string{},
	}
	if v, err := op.SettingGetInt(model.SettingKeyModelInfoUpdateInterval); err == nil {
		st.IntervalHours = v
	}
	runs, err := op.PriceSyncRunList(ctx, 10)
	if err != nil {
		return st, err
	}
	st.Runs = runs
	lastAutoRunM.RLock()
	last := lastAutoRun
	lastAutoRunM.RUnlock()
	if st.AutoEnabled && st.IntervalHours > 0 && !last.IsZero() {
		next := last.Add(time.Duration(st.IntervalHours) * time.Hour)
		st.NextRunAt = &next
	}
	for _, r := range runs {
		if r.Trigger != model.PriceSyncTriggerAuto || r.Status != model.PriceSyncStatusOK {
			continue
		}
		for _, name := range strings.Split(r.HeldModels, "\n") {
			if name == "" {
				continue
			}
			info, ok := op.LLMGetInfo(name)
			lib, okLib := CatalogPrice(name)
			if !ok || !okLib || info.Source == model.LLMSourceManual {
				continue
			}
			if !info.LLMPrice.SameTokenPrice(lib) && IsBigChange(info.LLMPrice, lib) {
				st.HeldPending = append(st.HeldPending, name)
			}
		}
		break // only the newest successful auto run counts
	}
	return st, nil
}
