package balancer

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// A hold is a cooldown with a fixed end, set by an operator error rule rather than by the
// consecutive-failure breaker. It lives beside the breaker so the routing check, the admin
// list, reset and Retry-After all see one picture.
//
// Hold keys reuse the breaker layout channelID:keyID:model with two wildcards:
//   - channel:key:model  scope model   (this model on this key)
//   - channel:key:*      scope key     (every model on this key)
//   - channel:0:*        scope channel (every key and model)
const holdAnyModel = "*"

type holdEntry struct {
	until time.Time
	rule  string
}

var globalHolds sync.Map // key: string -> holdEntry

func holdKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("%d:%d:%s", channelID, keyID, modelName)
}

// ApplyHold holds the scope until now+cooldown. A concurrent or repeated hold only ever
// extends the end, never shortens it.
func ApplyHold(channelID, keyID int, modelName, scope string, cooldown time.Duration, rule string) {
	if cooldown <= 0 {
		return
	}
	var key string
	switch scope {
	case model.CircuitRuleScopeModel:
		key = holdKey(channelID, keyID, modelName)
	case model.CircuitRuleScopeKey:
		key = holdKey(channelID, keyID, holdAnyModel)
	case model.CircuitRuleScopeChannel:
		key = holdKey(channelID, 0, holdAnyModel)
	default:
		return
	}
	until := time.Now().Add(cooldown)
	for {
		current, loaded := globalHolds.LoadOrStore(key, holdEntry{until: until, rule: rule})
		if !loaded {
			break
		}
		existing := current.(holdEntry)
		if !until.After(existing.until) {
			break
		}
		if globalHolds.CompareAndSwap(key, existing, holdEntry{until: until, rule: rule}) {
			break
		}
	}
	log.Warnf("circuit hold [%s] scope=%s rule=%q for %v", key, scope, rule, cooldown)
}

// holdRemaining returns the longest active hold covering channel/key/model.
func holdRemaining(channelID, keyID int, modelName string) (time.Duration, string) {
	now := time.Now()
	var longest time.Duration
	var rule string
	for _, key := range []string{
		holdKey(channelID, keyID, modelName),
		holdKey(channelID, keyID, holdAnyModel),
		holdKey(channelID, 0, holdAnyModel),
	} {
		value, ok := globalHolds.Load(key)
		if !ok {
			continue
		}
		entry := value.(holdEntry)
		left := entry.until.Sub(now)
		if left <= 0 {
			globalHolds.CompareAndDelete(key, entry)
			continue
		}
		if left > longest {
			longest = left
			rule = entry.rule
		}
	}
	return longest, rule
}

// clearHolds removes holds narrowed like ResetScoped. A key or model reset keeps wider
// holds (a key reset keeps the channel-wide hold; a model reset keeps key-wide holds),
// since those were not about that key or model.
func clearHolds(channelID, keyID int, modelName string) int {
	cleared := 0
	globalHolds.Range(func(key, _ any) bool {
		parsed, ok := parseCircuitKey(key.(string))
		if !ok || parsed.channelID != channelID {
			return true
		}
		if keyID > 0 && parsed.keyID != keyID {
			return true
		}
		if modelName != "" && parsed.model != modelName {
			return true
		}
		globalHolds.Delete(key)
		cleared++
		return true
	})
	return cleared
}

// listHolds returns the channel's active holds as admin-list rows.
func listHolds(channelID int) []CircuitEntryView {
	now := time.Now()
	views := make([]CircuitEntryView, 0)
	globalHolds.Range(func(key, value any) bool {
		parsed, ok := parseCircuitKey(key.(string))
		if !ok || parsed.channelID != channelID {
			return true
		}
		entry := value.(holdEntry)
		left := entry.until.Sub(now)
		if left <= 0 {
			return true
		}
		views = append(views, CircuitEntryView{
			KeyID:            parsed.keyID,
			Model:            parsed.model,
			State:            "held",
			RemainingSeconds: int((left + time.Second - 1) / time.Second),
			Reason:           entry.rule,
		})
		return true
	})
	return views
}

type compiledCircuitRules struct {
	raw   string
	rules []model.CircuitErrorRule
}

var circuitRulesCache sync.Map // single key "rules" -> compiledCircuitRules

// circuitRulesSource reads the raw rule setting; tests swap it to avoid the settings DB.
var circuitRulesSource = func() (string, error) {
	return op.SettingGetString(model.SettingKeyCircuitBreakerErrorRules)
}

func currentCircuitRules() []model.CircuitErrorRule {
	raw, err := circuitRulesSource()
	if err != nil {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	if cached, ok := circuitRulesCache.Load("rules"); ok {
		compiled := cached.(compiledCircuitRules)
		if compiled.raw == raw {
			return compiled.rules
		}
	}
	rules, err := model.ParseCircuitErrorRules(raw)
	if err != nil {
		log.Warnf("ignoring invalid circuit error rules: %v", err)
		rules = nil
	}
	circuitRulesCache.Store("rules", compiledCircuitRules{raw: raw, rules: rules})
	return rules
}

// ApplyErrorRules checks a counted failure against the operator rules and holds the first
// matching rule's scope. It returns the matched rule name, or "" when none matched.
func ApplyErrorRules(channelID, keyID int, modelName string, statusCode int, errText string) string {
	rules := currentCircuitRules()
	if len(rules) == 0 {
		return ""
	}
	lower := strings.ToLower(errText)
	for _, rule := range rules {
		if !rule.Match(statusCode, lower) {
			continue
		}
		ApplyHold(channelID, keyID, modelName, rule.Scope, time.Duration(rule.CooldownSeconds)*time.Second, rule.Name)
		return rule.Name
	}
	return ""
}
