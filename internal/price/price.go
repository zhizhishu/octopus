package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/client"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

const llmPriceUrl = "https://models.dev/api.json"

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

var lastUpdateTime time.Time

func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	client, err := client.GetHTTPClientSystemProxy(false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	var rawPrice map[string]struct {
		Models map[string]struct {
			ID   string         `json:"id"`
			Cost model.LLMPrice `json:"cost"`
		} `json:"models"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}
	llmPriceLock.Lock()
	for _, provider := range Provider {
		for _, model := range rawPrice[provider].Models {
			model.ID = strings.ToLower(model.ID)
			llmPrice[model.ID] = model.Cost
		}
	}
	llmPriceLock.Unlock()
	lastUpdateTime = time.Now()
	return nil
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
