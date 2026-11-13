package upstream

import (
	"encoding/json"
	"strings"
)

// KimiToolProtocolPrompt stabilizes native tool_calls output for Qoder Kimi models.
const KimiToolProtocolPrompt = `[Tool protocol requirements]
When tools are available, use the native tool_calls channel for actual tool calls.
Historical tool-call text and tool-result messages are records, not output templates.
Do not repeat textual tool-call markers, XML/DSML tags, JSON call envelopes, or Markdown fences as a substitute for a native tool call. Ordinary answers remain ordinary text; the presence of tools does not require calling one.
For each tool call, arguments must encode one complete valid JSON object matching that tool's declared parameters, required fields and value types.
Before emitting the call, check full JSON syntax and schema; never truncate a JSON string/object. Do not claim execution until a tool result arrives.
[/Tool protocol requirements]`

// NormalizeTools coerces top-level function definitions (e.g. Codex /v1/responses format)
// into standard OpenAI nested {"type":"function","function":{...}} objects accepted by Qoder.
func NormalizeTools(raw any) []any {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	var out []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(anyStr(m["type"]))
		if typ != "" && typ != "function" {
			// Skip unsupported built-in tool pseudo-types (namespace/web_search/custom_freeform).
			if typ == "custom" {
				if name := strings.TrimSpace(anyStr(m["name"])); name != "" {
					out = append(out, map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":        name,
							"description": anyStr(m["description"]),
							"parameters": map[string]any{
								"type":       "object",
								"properties": map[string]any{"input": map[string]any{"type": "string"}},
								"required":   []string{"input"},
							},
						},
					})
				}
			}
			continue
		}
		if fn, ok := m["function"].(map[string]any); ok {
			out = append(out, map[string]any{
				"type":     "function",
				"function": fn,
			})
			continue
		}
		// Flat function schema: {"type":"function", "name":..., "parameters":...}
		if name := strings.TrimSpace(anyStr(m["name"])); name != "" {
			fn := map[string]any{"name": name}
			if d := anyStr(m["description"]); d != "" {
				fn["description"] = d
			}
			if p, exists := m["parameters"]; exists && p != nil {
				fn["parameters"] = p
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
	}
	return out
}

// NormalizeToolChoice normalizes OpenAI/Anthropic tool_choice variants for Qoder upstream.
func NormalizeToolChoice(raw any) any {
	if raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok {
		low := strings.ToLower(strings.TrimSpace(s))
		switch low {
		case "auto", "none", "required":
			return low
		}
		return s
	}
	if m, ok := raw.(map[string]any); ok {
		typ := strings.ToLower(strings.TrimSpace(anyStr(m["type"])))
		switch typ {
		case "auto", "any":
			return "auto"
		case "none":
			return "none"
		case "required":
			return "required"
		case "function", "tool":
			if fn, ok := m["function"].(map[string]any); ok {
				if name := strings.TrimSpace(anyStr(fn["name"])); name != "" {
					return name
				}
			}
			if name := strings.TrimSpace(anyStr(m["name"])); name != "" {
				return name
			}
			return "auto"
		}
	}
	return nil
}

// NormalizeToolCalls coerces assistant tool_calls so arguments are always strings and ids are present.
func NormalizeToolCalls(raw any) []any {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	var out []any
	for _, item := range list {
		tc, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name := ""
		var argsStr string
		if fn != nil {
			name = anyStr(fn["name"])
			switch a := fn["arguments"].(type) {
			case string:
				argsStr = a
			case nil:
				argsStr = "{}"
			default:
				if b, err := json.Marshal(a); err == nil {
					argsStr = string(b)
				} else {
					argsStr = "{}"
				}
			}
		}
		if argsStr == "" {
			argsStr = "{}"
		}
		id := strings.TrimSpace(anyStr(tc["id"]))
		typ := strings.TrimSpace(anyStr(tc["type"]))
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": typ,
			"function": map[string]any{
				"name":      name,
				"arguments": argsStr,
			},
		})
	}
	return out
}

// RepairToolPairing ensures assistant tool_call IDs match subsequent role:"tool" messages
// and drops orphan tool results so Qoder's DeepSeek/Claude converters never throw 400 provider_error.
func RepairToolPairing(msgs []map[string]any) []map[string]any {
	validCallIDs := make(map[string]bool)
	for _, m := range msgs {
		if m == nil || !strings.EqualFold(anyStr(m["role"]), "assistant") {
			continue
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, item := range tcs {
				if tc, ok := item.(map[string]any); ok {
					if id := strings.TrimSpace(anyStr(tc["id"])); id != "" {
						validCallIDs[id] = true
					}
				}
			}
		}
	}

	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		role := strings.ToLower(anyStr(m["role"]))
		if role == "tool" {
			cid := strings.TrimSpace(anyStr(m["tool_call_id"]))
			if cid != "" && !validCallIDs[cid] {
				// Orphan tool result -> fold into plain user observation rather than sending broken tool_call_id.
				out = append(out, map[string]any{
					"role":    "user",
					"content": "[Tool Output]: " + anyStr(m["content"]),
				})
				continue
			}
			if m["content"] == nil {
				m["content"] = ""
			}
		}
		if role == "assistant" {
			if _, hasTC := m["tool_calls"]; hasTC && m["content"] == nil {
				m["content"] = ""
			}
		}
		out = append(out, m)
	}
	return out
}

// ApplyKimiToolProtocol appends strict JSON tool instructions to system prompt for Kimi models when tools are present.
func ApplyKimiToolProtocol(messages []map[string]any, model, modelKey string, hasTools bool, toolChoice any) []map[string]any {
	if !hasTools || !IsKimiModel(model, modelKey) {
		return messages
	}
	if s, ok := toolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
		return messages
	}
	for _, m := range messages {
		if m != nil && strings.EqualFold(anyStr(m["role"]), "system") {
			cur := anyStr(m["content"])
			if !strings.Contains(cur, "[Tool protocol requirements]") {
				if cur != "" {
					m["content"] = cur + "\n\n" + KimiToolProtocolPrompt
				} else {
					m["content"] = KimiToolProtocolPrompt
				}
			}
			return messages
		}
	}
	sysMsg := map[string]any{"role": "system", "content": KimiToolProtocolPrompt}
	return append([]map[string]any{sysMsg}, messages...)
}
