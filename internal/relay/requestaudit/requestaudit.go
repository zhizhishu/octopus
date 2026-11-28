// Package requestaudit 发上游前的决策模型风控审查（feat/request-audit）。
// 照抄 gpt-load internal/requestaudit（audit.go / review.go / content.go）的形状：
//   - 配置形状（enabled + rules[{id,name,enabled,instructions,action,threshold}]，
//     默认 action=block、threshold=0.8）照 audit.go Rule/Config；
//   - sha256 指纹缓存 1 小时、不存原文，照 review.go hashParts + evidence cache；
//   - 24KB 取样、2 秒超时、fail-open（失败放行并标「未审查」）照 decisions-api-1128.md §4。
//
// 与 gpt-load 的差异：取样不做头中尾三段摘录，按工单直接取请求正文前 24KB；
// 审查调用走 octopus 自己的渠道闸门/RPM（见 upstream.go），不走 gpt-load 的 group 体系。
package requestaudit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const SettingKey = "request_audit"

// MaxSampleBytes 审计取样上限：请求正文前 24KB（工单口径；gpt-load audit.go
// MaxRequestBytes 同为 24<<10，为其 JEV 32k token 上下文的保守预算）。
const MaxSampleBytes = 24 << 10

const (
	ActionBlock = "block"
	ActionWarn  = "warn"
)

var ErrInvalidConfig = errors.New("invalid request audit configuration")

type Rule struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Enabled      bool    `json:"enabled"`
	Instructions string  `json:"instructions"`
	Action       string  `json:"action"`    // block | warn
	Threshold    float64 `json:"threshold"` // 命中概率达到该值即触发；默认 0.8
}

func (r *Rule) UnmarshalJSON(raw []byte) error {
	type plain Rule
	value := plain{Enabled: true, Action: ActionBlock, Threshold: 0.8}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = Rule(value)
	return nil
}

type Config struct {
	Enabled   bool   `json:"enabled"`
	ChannelID int    `json:"channel_id"` // 审查走的上游渠道（如生产 68）；0=未配置
	Rules     []Rule `json:"rules"`
}

// ParseConfig 解析并校验设置值。校验口径照 gpt-load audit.go validate：
// 规则 id 唯一非空、instructions 非空、action ∈ {block,warn}、threshold ∈ (0,1]。
func ParseConfig(raw string) (*Config, error) {
	cfg := &Config{}
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	seen := map[string]bool{}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		r.ID = strings.TrimSpace(r.ID)
		if r.ID == "" {
			return nil, fmt.Errorf("%w: rule %d has empty id", ErrInvalidConfig, i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%w: duplicate rule id %q", ErrInvalidConfig, r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Instructions) == "" {
			return nil, fmt.Errorf("%w: rule %q has empty instructions", ErrInvalidConfig, r.ID)
		}
		if r.Action != ActionBlock && r.Action != ActionWarn {
			return nil, fmt.Errorf("%w: rule %q action must be block or warn", ErrInvalidConfig, r.ID)
		}
		if r.Threshold <= 0 || r.Threshold > 1 {
			return nil, fmt.Errorf("%w: rule %q threshold must be in (0,1]", ErrInvalidConfig, r.ID)
		}
	}
	return cfg, nil
}

func (c *Config) activeRules() []Rule {
	var out []Rule
	for _, r := range c.Rules {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

// Sample 取请求正文前 24KB（工单口径），不足则全量。
func Sample(body []byte) []byte {
	if len(body) <= MaxSampleBytes {
		return body
	}
	return body[:MaxSampleBytes]
}

// fingerprint 照 gpt-load review.go hashParts：长度前缀+分段 sha256，只存哈希不存原文。
func fingerprint(sample []byte, ruleIDs []string) [32]byte {
	h := sha256.New()
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(sample)))
	h.Write(length[:])
	h.Write(sample)
	for _, id := range ruleIDs {
		binary.LittleEndian.PutUint64(length[:], uint64(len(id)))
		h.Write(length[:])
		h.Write([]byte(id))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

const cacheTTL = time.Hour

type fingerprintCache struct {
	mu      sync.Mutex
	entries map[[32]byte]cacheEntry
	order   [][32]byte // 简单 FIFO 驱逐，上限 4096 条防内存膨胀
}

type cacheEntry struct {
	verdicts map[string]float64 // ruleID -> probability
	expires  time.Time
}

const cacheMaxEntries = 4096

func newFingerprintCache() *fingerprintCache {
	return &fingerprintCache{entries: map[[32]byte]cacheEntry{}}
}

func (c *fingerprintCache) get(key [32]byte, now time.Time) (map[string]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.After(entry.expires) {
		return nil, false
	}
	out := make(map[string]float64, len(entry.verdicts))
	for k, v := range entry.verdicts {
		out[k] = v
	}
	return out, true
}

func (c *fingerprintCache) put(key [32]byte, verdicts map[string]float64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		c.order = append(c.order, key)
		for len(c.order) > cacheMaxEntries {
			victim := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, victim)
		}
	}
	cp := make(map[string]float64, len(verdicts))
	for k, v := range verdicts {
		cp[k] = v
	}
	c.entries[key] = cacheEntry{verdicts: cp, expires: now.Add(cacheTTL)}
}

type Finding struct {
	RuleID      string  `json:"rule_id"`
	Name        string  `json:"name"`
	Action      string  `json:"action"`
	Probability float64 `json:"probability"`
}

type Outcome struct {
	Audited  bool      `json:"audited"` // false=未审查（fail-open 放行）
	Blocked  bool      `json:"blocked"`
	CacheHit bool      `json:"cache_hit"`
	Findings []Finding `json:"findings,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// DecisionsCaller 生产实现见 upstream.go（走渠道闸门/RPM/2 秒超时）；
// 测试注入 mock，绝不发真实请求。
type DecisionsCaller func(ctx context.Context, sc SampleContext, questions []Question) (map[string]float64, error)

// SampleContext 导出以便 relay 层与测试注入 caller。
type SampleContext struct {
	ChannelID int
	Sample    []byte
	Model     string
}

type Question struct {
	RuleID string
	Query  string
}

// buildInput 组装 decisions 的 input：管理员自然语言 instructions 包进问题，
// 请求样本原文放入。问题用 predicate 题型（概率），照 decisions-api-1128.md §1。
func buildQuestions(rules []Rule) []Question {
	out := make([]Question, 0, len(rules))
	for _, r := range rules {
		out = append(out, Question{RuleID: r.ID, Query: strings.TrimSpace(r.Instructions)})
	}
	return out
}

type Auditor struct {
	caller DecisionsCaller
	cache  *fingerprintCache
	now    func() time.Time
}

func NewAuditor(caller DecisionsCaller) *Auditor {
	return &Auditor{caller: caller, cache: newFingerprintCache(), now: time.Now}
}

// Audit 对一段请求正文执行全部启用规则。任何失败/超时 → fail-open：Audited=false 放行。
func (a *Auditor) Audit(ctx context.Context, cfg *Config, channelID int, model string, body []byte) Outcome {
	if cfg == nil || !cfg.Enabled {
		return Outcome{Audited: true}
	}
	rules := cfg.activeRules()
	if len(rules) == 0 {
		return Outcome{Audited: true}
	}
	sample := Sample(body)
	ruleIDs := make([]string, len(rules))
	for i, r := range rules {
		ruleIDs[i] = r.ID
	}
	fp := fingerprint(sample, ruleIDs)
	if verdicts, ok := a.cache.get(fp, a.now()); ok {
		return finishOutcome(rules, verdicts, true)
	}
	if a.caller == nil {
		return Outcome{Audited: false, Reason: "no caller"}
	}
	verdicts, err := a.caller(ctx, SampleContext{ChannelID: channelID, Sample: sample, Model: model}, buildQuestions(rules))
	if err != nil {
		// fail-open：超时/上游失败都放行，调用方负责记「未审查」日志。
		return Outcome{Audited: false, Reason: err.Error()}
	}
	a.cache.put(fp, verdicts, a.now())
	return finishOutcome(rules, verdicts, false)
}

func finishOutcome(rules []Rule, verdicts map[string]float64, cacheHit bool) Outcome {
	out := Outcome{Audited: true, CacheHit: cacheHit}
	byID := make(map[string]Rule, len(rules))
	for _, r := range rules {
		byID[r.ID] = r
	}
	for id, p := range verdicts {
		rule, ok := byID[id]
		if !ok || p < rule.Threshold {
			continue
		}
		out.Findings = append(out.Findings, Finding{RuleID: id, Name: rule.Name, Action: rule.Action, Probability: p})
		if rule.Action == ActionBlock {
			out.Blocked = true
		}
	}
	return out
}
