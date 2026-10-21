package anthropic

import "github.com/zhizhishu/octopus/internal/transformer/model"

// thinkingBudgetToReasoningEffort converts thinking budget tokens to a reasoning
// effort string, using the shared effort ladder in transformer/model.
//
// Before this, the function had its own thresholds (5000/15000, else high) that
// matched nothing else in the codebase, and — more importantly — could never
// produce "max" no matter how large the budget was. A client asking for
// budget_tokens=100000 was silently reduced to high; Anthropic→Anthropic hid the
// loss because ReasoningBudget still carried the original number, but any
// cross-protocol hop exposed it.
func thinkingBudgetToReasoningEffort(budgetTokens int64) string {
	if effort := model.EffortFromBudget(budgetTokens); effort != "" {
		return effort
	}
	// A non-positive budget is not a valid request; treat it as the lowest
	// explicit level rather than inventing an empty effort (which downstream
	// transformers read as "client said nothing").
	return EffortLow
}
