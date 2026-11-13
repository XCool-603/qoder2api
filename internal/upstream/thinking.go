package upstream

import "strings"

// RequestedEffort extracts the raw client thinking/reasoning intensity in Qoder's canonical order:
// reasoning_effort -> reasoning.effort -> thinking.effort / thinking.level / thinking.budget_tokens.
func RequestedEffort(req map[string]any) string {
	if req == nil {
		return ""
	}
	if e := strings.TrimSpace(anyStr(req["reasoning_effort"])); e != "" {
		return e
	}
	if r, ok := req["reasoning"].(map[string]any); ok {
		if e := strings.TrimSpace(anyStr(r["effort"])); e != "" {
			return e
		}
	}
	if th, ok := req["thinking"].(map[string]any); ok {
		typ := strings.ToLower(strings.TrimSpace(anyStr(th["type"])))
		if typ == "disabled" {
			return "none"
		}
		if e := strings.TrimSpace(anyStr(th["effort"])); e != "" {
			return e
		}
		if e := strings.TrimSpace(anyStr(th["level"])); e != "" {
			return e
		}
		if bt := anyInt(th["budget_tokens"]); bt > 0 {
			return BudgetTokensToEffort(bt)
		}
		if typ == "enabled" {
			return "high"
		}
	}
	return ""
}

// BudgetTokensToEffort translates Anthropic thinking.budget_tokens into Qoder effort levels.
func BudgetTokensToEffort(budget int) string {
	switch {
	case budget <= 1024:
		return "minimal"
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	case budget <= 32768:
		return "high"
	default:
		return "max"
	}
}

// BackfillReasoningContent guarantees DeepSeek assistant history turns preserve reasoning_content
// whenever any turn carries reasoning/reasoning_content so multi-turn tool calls don't fail upstream.
func BackfillReasoningContent(messages []map[string]any, model, modelKey string) []map[string]any {
	if !IsDeepSeekModel(model, modelKey) {
		return messages
	}
	hasTrace := false
	for _, m := range messages {
		if m == nil {
			continue
		}
		if _, ok := m["reasoning_content"]; ok || m["reasoning"] != nil {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return messages
	}
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		if m == nil {
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if strings.EqualFold(anyStr(cp["role"]), "assistant") {
			if _, exists := cp["reasoning_content"]; !exists {
				if r := anyStr(cp["reasoning"]); r != "" {
					cp["reasoning_content"] = r
				} else {
					cp["reasoning_content"] = ""
				}
			} else if cp["reasoning_content"] == nil {
				cp["reasoning_content"] = ""
			}
		}
		out[i] = cp
	}
	return out
}
