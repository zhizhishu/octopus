package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// fakeModelsDev is a stand-in for models.dev. Go tests never fetch the live library.
type fakeModelsDev struct {
	mu       sync.Mutex
	prices   map[string]map[string]model.LLMPrice // provider -> id -> price
	status   int
	fillers  int
	requests []*http.Request
	srv      *httptest.Server
}

const fakeChannelKey = "sk-ps-secret-channel-key"

func newFakeModelsDev(t *testing.T) *fakeModelsDev {
	t.Helper()
	f := &fakeModelsDev{prices: map[string]map[string]model.LLMPrice{}, status: http.StatusOK, fillers: 80}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		out := map[string]map[string]map[string]any{}
		add := func(provider, id string, p model.LLMPrice) {
			if out[provider] == nil {
				out[provider] = map[string]map[string]any{"models": {}}
			}
			out[provider]["models"][id] = map[string]any{"id": id, "cost": map[string]float64{
				"input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_write": p.CacheWrite,
			}}
		}
		for i := 0; i < f.fillers; i++ {
			add("openai", fmt.Sprintf("ps-filler-%03d", i), model.LLMPrice{Input: 1, Output: 2})
		}
		for provider, models := range f.prices {
			for id, p := range models {
				add(provider, id, p)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	prev := price.ModelsDevURL
	price.ModelsDevURL = f.srv.URL + "/api.json"
	t.Cleanup(func() { price.ModelsDevURL = prev })
	return f
}

func (f *fakeModelsDev) set(provider, id string, p model.LLMPrice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prices[provider] == nil {
		f.prices[provider] = map[string]model.LLMPrice{}
	}
	f.prices[provider][id] = p
}

func (f *fakeModelsDev) assertNoSecrets(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatalf("fake models.dev was never called")
	}
	for _, r := range f.requests {
		dump := r.URL.String()
		for k, v := range r.Header {
			dump += k + ":" + strings.Join(v, ",")
		}
		if strings.Contains(dump, fakeChannelKey) || r.Header.Get("Authorization") != "" {
			t.Fatalf("library fetch leaked a credential: %s", dump)
		}
	}
}

func createPriceSyncChannel(t *testing.T, ctx context.Context, models ...string) {
	t.Helper()
	ch := model.Channel{
		Name:           "ps-channel",
		Type:           outbound.OutboundTypeOpenAIChat,
		Enabled:        true,
		BaseUrls:       []model.BaseUrl{{URL: "https://upstream.example"}},
		Keys:           []model.ChannelKey{{Enabled: true, ChannelKey: fakeChannelKey}},
		SelectedModels: models,
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

// putRow writes a price row straight to the DB (bypassing handlers) and reloads the
// cache, the way an old database or the model sync would have left it.
func putRow(t *testing.T, ctx context.Context, name string, p model.LLMPrice, source string) {
	t.Helper()
	row := model.LLMInfo{Name: name, LLMPrice: p, Source: source}
	if err := db.GetDB().Save(&row).Error; err != nil {
		t.Fatalf("save row: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("reload cache: %v", err)
	}
}

func callHandler(t *testing.T, h gin.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("{}")
	} else {
		reader = strings.NewReader(body)
	}
	if path == "" {
		path = "/"
	}
	c.Request = httptest.NewRequest(method, path, reader)
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	return w
}

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return env.Data
}

func previewViaHandler(t *testing.T) price.SyncPreview {
	t.Helper()
	w := callHandler(t, previewPriceSync, http.MethodPost, "/api/v1/model/price-sync/preview", "")
	if w.Code != http.StatusOK {
		t.Fatalf("preview status %d body=%s", w.Code, w.Body.String())
	}
	return decodeData[price.SyncPreview](t, w)
}

func itemsByName(p price.SyncPreview) map[string]price.SyncItem {
	out := map[string]price.SyncItem{}
	for _, it := range p.Items {
		out[it.Name] = it
	}
	return out
}

func p4(in, out, cr, cw float64) model.LLMPrice {
	return model.LLMPrice{Input: in, Output: out, CacheRead: cr, CacheWrite: cw}
}

func setupPriceSync(t *testing.T) (context.Context, *fakeModelsDev) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	return ctx, newFakeModelsDev(t)
}

func TestPriceSyncPreviewDiffKinds(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("openai", "ps-new", p4(5, 25, 0.5, 6.25))
	f.set("openai", "ps-changed", p4(4, 20, 0.4, 5))
	f.set("openai", "ps-big", p4(25, 125, 2.5, 31.25))
	f.set("openai", "ps-manual", p4(1.4, 4.4, 0.28, 0))
	f.set("openai", "ps-same", p4(2, 8, 0.2, 0))
	createPriceSyncChannel(t, ctx, "ps-new", "ps-changed", "ps-big", "ps-manual", "ps-same", "ps-nolib", "ps-norow")
	f.set("openai", "ps-norow", p4(1, 1, 0, 0))
	putRow(t, ctx, "ps-new", model.LLMPrice{PerCall: 0.4}, model.LLMSourceLibrary)
	putRow(t, ctx, "ps-changed", p4(3, 15, 0.3, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-big", p4(10, 50, 1, 12.5), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-manual", p4(1, 3.2, 0.2, 0), model.LLMSourceManual)
	putRow(t, ctx, "ps-same", p4(2, 8, 0.2, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-nolib", model.LLMPrice{}, model.LLMSourceLibrary)
	// A row that is not in any channel must stay out of the preview.
	f.set("openai", "ps-not-in-channel", p4(9, 9, 0, 0))
	putRow(t, ctx, "ps-not-in-channel", p4(1, 1, 0, 0), model.LLMSourceLibrary)

	preview := previewViaHandler(t)
	f.assertNoSecrets(t)
	if preview.Total != 7 || preview.NoLibrary != 1 {
		t.Fatalf("total=%d no_library=%d, want 7/1", preview.Total, preview.NoLibrary)
	}
	items := itemsByName(preview)
	want := map[string]struct {
		kind   string
		big    bool
		exists bool
	}{
		"ps-new":     {price.SyncKindNew, false, true},
		"ps-norow":   {price.SyncKindNew, false, false},
		"ps-changed": {price.SyncKindChanged, false, true},
		"ps-big":     {price.SyncKindChanged, true, true},
		"ps-manual":  {price.SyncKindManual, false, true},
		"ps-same":    {price.SyncKindSame, false, true},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items %+v, want %d", len(items), items, len(want))
	}
	for name, w := range want {
		it, ok := items[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if it.Kind != w.kind || it.Big != w.big || it.Exists != w.exists {
			t.Fatalf("%s: kind=%s big=%v exists=%v, want %+v", name, it.Kind, it.Big, it.Exists, w)
		}
	}
	if !items["ps-new"].HasPerCall {
		t.Fatalf("ps-new should report its per-call price")
	}
	// Preview writes nothing.
	if row := loadLLMRow(t, "ps-changed"); !row.SameTokenPrice(p4(3, 15, 0.3, 0)) {
		t.Fatalf("preview wrote a row: %+v", row)
	}
}

func applyBody(t *testing.T, items ...price.SyncItem) string {
	t.Helper()
	changes := make([]price.SyncApplyChange, 0, len(items))
	for _, it := range items {
		changes = append(changes, price.SyncApplyChange{Name: it.Name, Source: it.Source, Exists: it.Exists, Current: it.Current, Next: it.Next})
	}
	b, _ := json.Marshal(map[string]any{"changes": changes})
	return string(b)
}

func TestPriceSyncApplyConflictAndPerCall(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("openai", "ps-a", p4(4, 20, 0.4, 5))
	f.set("openai", "ps-b", p4(2, 12, 0.2, 0))
	f.set("openai", "ps-m", p4(1.4, 4.4, 0.28, 0))
	createPriceSyncChannel(t, ctx, "ps-a", "ps-b", "ps-m", "ps-c")
	f.set("openai", "ps-c", p4(1, 2, 0, 0))
	putRow(t, ctx, "ps-a", model.LLMPrice{Input: 3, Output: 15, CacheRead: 0.3, PerCall: 0.25}, model.LLMSourceLibrary)
	putRow(t, ctx, "ps-b", p4(1, 6, 0.1, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-m", p4(1, 3.2, 0.2, 0), model.LLMSourceManual)

	items := itemsByName(previewViaHandler(t))

	// Someone edits ps-b after the preview: the whole apply is refused with 409.
	if w := postUpdateLLM(t, `{"name":"ps-b","input":1.5}`); w.Code != http.StatusOK {
		t.Fatalf("edit status %d %s", w.Code, w.Body.String())
	}
	w := callHandler(t, applyPriceSync, http.MethodPost, "/api/v1/model/price-sync/apply", applyBody(t, items["ps-a"], items["ps-b"]))
	if w.Code != http.StatusConflict {
		t.Fatalf("apply after edit: status %d body=%s, want 409", w.Code, w.Body.String())
	}
	if row := loadLLMRow(t, "ps-a"); !row.SameTokenPrice(p4(3, 15, 0.3, 0)) {
		t.Fatalf("409 apply still wrote ps-a: %+v", row)
	}

	// The library moves after the preview: 409 as well.
	f.set("openai", "ps-a", p4(4.5, 20, 0.4, 5))
	if _, err := price.FetchLibrary(ctx); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if w := callHandler(t, applyPriceSync, http.MethodPost, "", applyBody(t, items["ps-a"])); w.Code != http.StatusConflict {
		t.Fatalf("apply after library moved: status %d, want 409", w.Code)
	}

	// Fresh preview: apply ps-a (changed), ps-c (no row yet) and the manual ps-m.
	items = itemsByName(previewViaHandler(t))
	w = callHandler(t, applyPriceSync, http.MethodPost, "", applyBody(t, items["ps-a"], items["ps-c"], items["ps-m"]))
	if w.Code != http.StatusOK {
		t.Fatalf("apply: status %d body=%s", w.Code, w.Body.String())
	}
	res := decodeData[price.SyncApplyResult](t, w)
	if res.Added != 1 || res.Changed != 2 {
		t.Fatalf("result %+v, want added 1 changed 2", res)
	}
	a := loadLLMRow(t, "ps-a")
	if !a.SameTokenPrice(p4(4.5, 20, 0.4, 5)) || a.PerCall != 0.25 || a.Source != model.LLMSourceLibrary {
		t.Fatalf("ps-a after apply: %+v (per_call must stay 0.25)", a)
	}
	if cached, _ := op.LLMGet("ps-a"); cached.PerCall != 0.25 {
		t.Fatalf("cache lost per_call: %+v", cached)
	}
	if c := loadLLMRow(t, "ps-c"); !c.SameTokenPrice(p4(1, 2, 0, 0)) || c.Source != model.LLMSourceLibrary {
		t.Fatalf("ps-c after apply: %+v", c)
	}
	// A ticked manual row goes back to following the library.
	if m := loadLLMRow(t, "ps-m"); m.Source != model.LLMSourceLibrary || !m.SameTokenPrice(p4(1.4, 4.4, 0.28, 0)) {
		t.Fatalf("ps-m after apply: %+v", m)
	}
}

func TestPriceEditMarksManualAndRestore(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("anthropic", "claude-ps", p4(5, 25, 0.5, 6.25))
	createPriceSyncChannel(t, ctx, "claude-ps")
	putRow(t, ctx, "claude-ps", model.LLMPrice{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25, PerCall: 0.1}, model.LLMSourceLibrary)
	previewViaHandler(t) // loads the library into the catalog

	// Editing only per_call keeps the source.
	if w := postUpdateLLM(t, `{"name":"claude-ps","per_call":0.2}`); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if row := loadLLMRow(t, "claude-ps"); row.Source != model.LLMSourceLibrary {
		t.Fatalf("per_call edit changed source: %+v", row)
	}
	if w := postUpdateLLM(t, `{"name":"claude-ps","input":6,"source":"library"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if row := loadLLMRow(t, "claude-ps"); row.Source != model.LLMSourceManual || row.Input != 6 {
		t.Fatalf("token edit should mark manual (client cannot pick the source): %+v", row)
	}
	w := callHandler(t, restoreLLMPrice, http.MethodPost, "", `{"name":"claude-ps"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("restore status %d %s", w.Code, w.Body.String())
	}
	row := loadLLMRow(t, "claude-ps")
	if row.Source != model.LLMSourceLibrary || !row.SameTokenPrice(p4(5, 25, 0.5, 6.25)) || row.PerCall != 0.2 {
		t.Fatalf("after restore: %+v", row)
	}
	putRow(t, ctx, "ps-unknown-x", p4(1, 1, 0, 0), model.LLMSourceManual)
	if w := callHandler(t, restoreLLMPrice, http.MethodPost, "", `{"name":"ps-unknown-x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("restore without library price: status %d, want 404", w.Code)
	}
}

func TestPriceAutoSyncRespectsManualAndHoldsBigChanges(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("openai", "ps-auto-new", p4(1, 2, 0.1, 0))
	f.set("openai", "ps-auto-chg", p4(4, 20, 0.4, 0))
	f.set("openai", "ps-auto-big", p4(25, 125, 2.5, 0))
	f.set("openai", "ps-auto-man", p4(9, 9, 0, 0))
	f.set("openai", "ps-auto-video", p4(1.2, 2.4, 0, 0))
	createPriceSyncChannel(t, ctx, "ps-auto-new", "ps-auto-chg", "ps-auto-big", "ps-auto-man", "ps-auto-video")
	putRow(t, ctx, "ps-auto-new", model.LLMPrice{}, model.LLMSourceLibrary)
	putRow(t, ctx, "ps-auto-chg", p4(3, 15, 0.3, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-auto-big", p4(10, 50, 1, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-auto-man", p4(1, 1, 0, 0), model.LLMSourceManual)
	putRow(t, ctx, "ps-auto-video", model.LLMPrice{Input: 1, Output: 2, PerCall: 0.5}, model.LLMSourceLibrary)

	run := price.AutoSync(ctx)
	if run.Status != model.PriceSyncStatusOK || run.Added != 1 || run.Changed != 2 || run.Held != 1 {
		t.Fatalf("run %+v, want ok added 1 changed 2 held 1", run)
	}
	f.assertNoSecrets(t)
	if r := loadLLMRow(t, "ps-auto-new"); !r.SameTokenPrice(p4(1, 2, 0.1, 0)) {
		t.Fatalf("new not applied: %+v", r)
	}
	if r := loadLLMRow(t, "ps-auto-chg"); !r.SameTokenPrice(p4(4, 20, 0.4, 0)) {
		t.Fatalf("changed not applied: %+v", r)
	}
	if r := loadLLMRow(t, "ps-auto-big"); !r.SameTokenPrice(p4(10, 50, 1, 0)) {
		t.Fatalf("big change was applied, should be held: %+v", r)
	}
	if r := loadLLMRow(t, "ps-auto-man"); !r.SameTokenPrice(p4(1, 1, 0, 0)) || r.Source != model.LLMSourceManual {
		t.Fatalf("manual row touched by auto sync: %+v", r)
	}
	if r := loadLLMRow(t, "ps-auto-video"); r.PerCall != 0.5 || !r.SameTokenPrice(p4(1.2, 2.4, 0, 0)) {
		t.Fatalf("video row: %+v (per_call must stay 0.5)", r)
	}

	w := callHandler(t, getPriceSyncStatus, http.MethodGet, "/api/v1/model/price-sync/status", "")
	st := decodeData[price.SyncStatus](t, w)
	if len(st.HeldPending) != 1 || st.HeldPending[0] != "ps-auto-big" || len(st.Runs) == 0 || st.Runs[0].Held != 1 {
		t.Fatalf("status %+v", st)
	}

	// With the hold off the big change goes through; the manual row still does not.
	if err := op.SettingSetString(model.SettingKeyPriceSyncHoldBigChange, "false"); err != nil {
		t.Fatal(err)
	}
	run = price.AutoSync(ctx)
	if run.Status != model.PriceSyncStatusOK || run.Changed != 1 || run.Held != 0 {
		t.Fatalf("second run %+v", run)
	}
	if r := loadLLMRow(t, "ps-auto-big"); !r.SameTokenPrice(p4(25, 125, 2.5, 0)) {
		t.Fatalf("big change not applied with hold off: %+v", r)
	}
	if r := loadLLMRow(t, "ps-auto-man"); r.Source != model.LLMSourceManual || r.Input != 1 {
		t.Fatalf("manual row touched: %+v", r)
	}
	st = decodeData[price.SyncStatus](t, callHandler(t, getPriceSyncStatus, http.MethodGet, "", ""))
	if len(st.HeldPending) != 0 {
		t.Fatalf("held pending should clear: %+v", st.HeldPending)
	}

	// Auto sync off: the catalog refreshes but no row changes and no run is recorded.
	if err := op.SettingSetString(model.SettingKeyPriceAutoSyncEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	f.set("openai", "ps-auto-chg", p4(4.4, 20, 0.4, 0))
	before := len(st.Runs)
	price.AutoSync(ctx)
	if r := loadLLMRow(t, "ps-auto-chg"); r.Input != 4 {
		t.Fatalf("auto sync off still wrote: %+v", r)
	}
	if runs, _ := op.PriceSyncRunList(ctx, 0); len(runs) != before {
		t.Fatalf("auto sync off recorded a run: %d -> %d", before, len(runs))
	}
}

func TestPriceAutoSyncAbortsOnFailedOrSmallFetch(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("openai", "ps-abort", p4(4, 20, 0, 0))
	createPriceSyncChannel(t, ctx, "ps-abort", "ps-abort-new")
	putRow(t, ctx, "ps-abort", p4(3, 15, 0, 0), model.LLMSourceLibrary)
	putRow(t, ctx, "ps-abort-new", model.LLMPrice{}, model.LLMSourceLibrary)
	f.set("openai", "ps-abort-new", p4(1, 1, 0, 0))
	// The catalog already knows a newer ps-abort price (e.g. from the built-in snapshot):
	// a broken fetch must not fall back to applying it.
	price.SetLLMPrice("ps-abort", p4(4, 20, 0, 0))
	t.Cleanup(func() { price.SetLLMPrice("ps-abort", model.LLMPrice{}) })

	f.status = http.StatusInternalServerError
	run := price.AutoSync(ctx)
	if run.Status != model.PriceSyncStatusFailed {
		t.Fatalf("failed fetch: %+v", run)
	}
	f.status = http.StatusOK
	f.fillers = 2 // far under MinLibraryModels
	run = price.AutoSync(ctx)
	if run.Status != model.PriceSyncStatusAborted {
		t.Fatalf("small fetch: %+v", run)
	}
	if r := loadLLMRow(t, "ps-abort"); !r.SameTokenPrice(p4(3, 15, 0, 0)) {
		t.Fatalf("aborted run wrote: %+v", r)
	}
	if r := loadLLMRow(t, "ps-abort-new"); !r.TokenPriceIsZero() {
		t.Fatalf("aborted run wrote: %+v", r)
	}
	if _, ok := price.CatalogPrice("ps-abort-new"); ok {
		t.Fatalf("a small fetch must not reach the catalog either")
	}
	runs, err := op.PriceSyncRunList(ctx, 0)
	if err != nil || len(runs) != 2 || runs[0].Status != model.PriceSyncStatusAborted || runs[1].Status != model.PriceSyncStatusFailed {
		t.Fatalf("history %+v err=%v", runs, err)
	}
	// The preview button reports the same failure instead of showing stale data.
	if w := callHandler(t, previewPriceSync, http.MethodPost, "", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("preview on small fetch: status %d", w.Code)
	}
}

func TestPriceSyncMigratesLegacyRows(t *testing.T) {
	ctx, f := setupPriceSync(t)
	f.set("openai", "ps-leg-eq", p4(2, 8, 0.2, 0))
	f.set("openai", "ps-leg-diff", p4(2, 8, 0.2, 0))
	f.set("openai", "ps-leg-zero", p4(2, 8, 0.2, 0))
	// gpt-4o ships in the built-in snapshot as 2.5/10/1.25/0; the fresh library moved.
	f.set("openai", "gpt-4o", p4(3, 12, 1.5, 0))
	createPriceSyncChannel(t, ctx, "ps-leg-eq", "ps-leg-diff", "ps-leg-zero", "gpt-4o")
	putRow(t, ctx, "ps-leg-eq", p4(2, 8, 0.2, 0), "")
	putRow(t, ctx, "ps-leg-diff", p4(1.5, 8, 0.2, 0), "")
	putRow(t, ctx, "ps-leg-zero", model.LLMPrice{PerCall: 0.3}, "")
	putRow(t, ctx, "gpt-4o", p4(2.5, 10, 1.25, 0), "")
	putRow(t, ctx, "ps-leg-manual-kept", p4(7, 7, 0, 0), model.LLMSourceManual)

	items := itemsByName(previewViaHandler(t))
	want := map[string]string{
		"ps-leg-eq":          model.LLMSourceLibrary,
		"ps-leg-diff":        model.LLMSourceManual,
		"ps-leg-zero":        model.LLMSourceLibrary,
		"gpt-4o":             model.LLMSourceLibrary,
		"ps-leg-manual-kept": model.LLMSourceManual,
	}
	for name, src := range want {
		if row := loadLLMRow(t, name); row.Source != src {
			t.Fatalf("%s classified %q, want %q", name, row.Source, src)
		}
	}
	if items["ps-leg-diff"].Kind != price.SyncKindManual || items["ps-leg-zero"].Kind != price.SyncKindNew || items["gpt-4o"].Kind != price.SyncKindChanged {
		t.Fatalf("kinds after migration: %+v", items)
	}
	// Classification never touches prices (per_call included).
	if row := loadLLMRow(t, "ps-leg-zero"); row.PerCall != 0.3 || !row.TokenPriceIsZero() {
		t.Fatalf("migration changed a price: %+v", row)
	}
}
