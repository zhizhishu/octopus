package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/client"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// ModelsDevURL is the price library. A var only so tests can point it at a fake server;
// Go tests never fetch the live library.
var ModelsDevURL = "https://models.dev/api.json"

// Provider is the fixed provider order used when the same model id appears under several
// providers. The model's own vendor always wins (see vendorForModel); otherwise the first
// provider in this order that has a non-zero price wins.
var Provider = []string{
	"openai",     // GPT 系列
	"anthropic",  // Claude 系列
	"google",     // Gemini 系列
	"deepseek",   // DeepSeek 系列
	"xai",        // Grok 系列
	"alibaba",    // Qwen 系列
	"zhipuai",    // GLM 系列
	"minimax",    // MiniMax 系列
	"moonshotai", // Kimi/Moonshot
	"v0",         // v0 系列
}

// vendorPrefixes maps a model-name prefix to the models.dev provider that makes it.
var vendorPrefixes = []struct {
	provider string
	prefixes []string
}{
	{"openai", []string{"gpt", "o1", "o3", "o4", "chatgpt", "text-embedding", "dall-e", "whisper", "tts-", "codex", "sora"}},
	{"anthropic", []string{"claude"}},
	{"google", []string{"gemini", "gemma", "imagen", "veo"}},
	{"deepseek", []string{"deepseek"}},
	{"xai", []string{"grok"}},
	{"alibaba", []string{"qwen", "qwq", "qvq", "wan"}},
	{"zhipuai", []string{"glm", "chatglm", "cogview"}},
	{"minimax", []string{"minimax", "abab"}},
	{"moonshotai", []string{"kimi", "moonshot"}},
	{"v0", []string{"v0-"}},
}

// vendorForModel returns the models.dev provider that makes this model, or "".
func vendorForModel(id string) string {
	id = strings.ToLower(id)
	for _, v := range vendorPrefixes {
		for _, p := range v.prefixes {
			if strings.HasPrefix(id, p) {
				return v.provider
			}
		}
	}
	return ""
}

var (
	// MinLibraryModels is the smallest library a fetch may return before it is treated
	// as broken. A var so tests can lower it.
	MinLibraryModels = 50
	// lastFetchedCount is the size of the previous good fetch; a fetch that comes back
	// under half of it is treated as broken too.
	lastFetchedCount int
	lastUpdateTime   time.Time
	fetchMu          sync.Mutex
)

// ErrLibraryTooSmall means models.dev answered but with far fewer priced models than
// expected (a partial or broken response). Nothing is written in that case.
var ErrLibraryTooSmall = errors.New("models.dev returned too few priced models")

type modelsDevCatalog map[string]struct {
	Models map[string]struct {
		ID   string         `json:"id"`
		Cost model.LLMPrice `json:"cost"`
	} `json:"models"`
}

// parseLibrary turns a models.dev catalog into lowercase-id -> token price. Only
// non-zero prices count. The model's own vendor wins; otherwise the fixed Provider
// order decides. Per-call prices never come from the library.
func parseLibrary(raw modelsDevCatalog) map[string]model.LLMPrice {
	out := make(map[string]model.LLMPrice)
	fromVendor := make(map[string]bool)
	for _, provider := range Provider {
		for key, m := range raw[provider].Models {
			id := strings.ToLower(strings.TrimSpace(m.ID))
			if id == "" {
				id = strings.ToLower(strings.TrimSpace(key))
			}
			if id == "" {
				continue
			}
			cost := m.Cost
			cost.PerCall = 0
			if cost.TokenPriceIsZero() || cost.Input < 0 || cost.Output < 0 || cost.CacheRead < 0 || cost.CacheWrite < 0 {
				continue
			}
			isVendor := vendorForModel(id) == provider
			if _, ok := out[id]; ok {
				if !isVendor || fromVendor[id] {
					continue
				}
			}
			out[id] = cost
			fromVendor[id] = isVendor
		}
	}
	return out
}

// FetchLibrary downloads models.dev through the system-proxy HTTP client (no channel
// key or other credential is sent), checks that the answer is not abnormally small,
// and merges it into the in-memory catalog. On any error the catalog is left as is.
func FetchLibrary(ctx context.Context) (map[string]model.LLMPrice, error) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	// The app's system-proxy client: through proxy_url when one is set, direct otherwise.
	proxyURL, _ := op.SettingGetString(model.SettingKeyProxyURL)
	httpClient, err := client.GetHTTPClientSystemProxy(strings.TrimSpace(proxyURL) != "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	var raw modelsDevCatalog
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse LLM info: %w", err)
	}
	lib := parseLibrary(raw)
	if len(lib) < MinLibraryModels || (lastFetchedCount > 0 && len(lib) < lastFetchedCount/2) {
		return nil, fmt.Errorf("%w: got %d", ErrLibraryTooSmall, len(lib))
	}
	llmPriceLock.Lock()
	for id, cost := range lib {
		llmPrice[id] = cost
	}
	llmPriceLock.Unlock()
	lastFetchedCount = len(lib)
	lastUpdateTime = time.Now()
	return lib, nil
}

// UpdateLLMPrice refreshes the in-memory catalog only. It never writes price rows.
func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	_, err := FetchLibrary(ctx)
	return err
}

// CatalogPrice is the library price for a model: the in-memory catalog (built-in
// snapshot merged with every good models.dev fetch), ignoring price rows. Like
// GetLLMPrice it retries a vendor-prefixed name ("openai/gpt-x") by its last segment.
func CatalogPrice(modelName string) (model.LLMPrice, bool) {
	name := strings.ToLower(strings.TrimSpace(modelName))
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	if p, ok := llmPrice[name]; ok && !p.TokenPriceIsZero() {
		return p, true
	}
	if i := strings.LastIndexAny(name, "/:"); i >= 0 && i+1 < len(name) {
		if p, ok := llmPrice[name[i+1:]]; ok && !p.TokenPriceIsZero() {
			return p, true
		}
	}
	return model.LLMPrice{}, false
}

func GetLastUpdateTime() time.Time {
	return lastUpdateTime
}

// SetLLMPrice 覆盖某个模型的内置价格（进程内，不落库）。
//
// 两个用途：
//  1. 测试要在不起 DB、不联外网的条件下构造价格场景（例如验证视频按次计费）。
//  2. 运维临时调整某个型号的价格而不想动 models.dev 目录。
//
// 注意 DB 里的 LLMInfo 行优先级更高（见 lookupLLMPrice）：这里改的是内置目录，
// 不是"最终生效价"。传入零值等于删除这条覆盖。
func SetLLMPrice(modelName string, p model.LLMPrice) {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if name == "" {
		return
	}
	llmPriceLock.Lock()
	defer llmPriceLock.Unlock()
	if p.IsZero() {
		delete(llmPrice, name)
		return
	}
	llmPrice[name] = p
}

func GetLLMPrice(modelName string) *model.LLMPrice {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if p := lookupLLMPrice(name); p != nil {
		return p
	}
	// Fallback: some upstreams report a vendor-prefixed model name (e.g.
	// "openai/gpt-x", "vendor:gpt-x") that isn't in the price table under its full
	// form. Retry with the last "/" or ":" segment so billing isn't silently zeroed
	// just because the upstream full name doesn't match the catalog key verbatim.
	if i := strings.LastIndexAny(name, "/:"); i >= 0 && i+1 < len(name) {
		if p := lookupLLMPrice(name[i+1:]); p != nil {
			return p
		}
	}
	return nil
}

func lookupLLMPrice(modelName string) *model.LLMPrice {
	// A DB row (manual override) wins ONLY when it carries a real price. A zero placeholder
	// row — auto-created when a model is added to a channel before it had a catalog price —
	// must NOT shadow the models.dev catalog, or the model bills at 0 forever even though
	// models.dev now prices it (e.g. gpt-5.6-sol). A deliberate free model would also have no
	// catalog entry, so it still resolves to nil/0 below.
	if price, err := op.LLMGet(modelName); err == nil && !price.IsZero() {
		return &price
	}
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	price, ok := llmPrice[modelName]
	if !ok {
		return nil
	}
	return &price
}
