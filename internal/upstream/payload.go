package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/prompt"
)

// MaxOutboundBodyBytes protects against Aliyun SLB 30MB hard RST limit (1MB safety margin).
const MaxOutboundBodyBytes = 29 * 1024 * 1024

// ChatMeta carries per-conversation continuity identifiers for upstream prefix caching & billing attribution.
type ChatMeta struct {
	ConversationID string
	BeginAtMs      int64
	SessionID      string
	PromptMode     string
	SystemPrompt   string
}

// BuildQoderPayload transforms an OpenAI-normalized request body into Qoder's
// /algo/api/v2/service/pro/sse/agent_chat_generation envelope ready for cosy.Encode.
func BuildQoderPayload(req map[string]any, a *auth.Auth, modelKey, realm string, meta ChatMeta) (map[string]any, error) {
	body := EmbeddedTemplate()
	keepReasoning := IsDeepSeekModel(anyStr(req["model"]), modelKey)

	rawMsgs, _ := req["messages"].([]any)
	var systemParts []string
	chatMsgs := make([]map[string]any, 0, len(rawMsgs))
	var latestPrompt string

	for _, raw := range rawMsgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := NormalizeRole(anyStr(m["role"]))
		txt, imgs, hasImg := FlattenContent(m["content"])

		if role == "system" {
			if txt != "" {
				systemParts = append(systemParts, txt)
			}
			continue
		}
		if role == "tool" {
			chatMsgs = append(chatMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": anyStr(m["tool_call_id"]),
				"content":      txt,
			})
			continue
		}
		if role == "assistant" {
			item := map[string]any{
				"role": "assistant",
			}
			if tcs := NormalizeToolCalls(m["tool_calls"]); len(tcs) > 0 {
				item["tool_calls"] = tcs
				item["content"] = txt // never nil; DeepSeek/Kimi require string ""
			} else {
				item["content"] = txt
			}
			if keepReasoning {
				if rc := anyStr(m["reasoning_content"]); rc != "" {
					item["reasoning_content"] = rc
				} else if r := anyStr(m["reasoning"]); r != "" {
					item["reasoning_content"] = r
				} else if _, exists := m["reasoning_content"]; exists {
					item["reasoning_content"] = ""
				}
			}
			chatMsgs = append(chatMsgs, item)
			continue
		}

		// User message (preserve structured image_url content blocks when VL images are attached)
		item := map[string]any{"role": "user"}
		if hasImg && len(imgs) > 0 {
			if parts, ok := m["content"].([]any); ok {
				item["content"] = parts
			} else {
				var blocks []any
				if txt != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": txt})
				}
				for _, u := range imgs {
					blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				}
				item["content"] = blocks
			}
		} else {
			item["content"] = txt
		}
		if txt != "" {
			latestPrompt = txt
		}
		chatMsgs = append(chatMsgs, item)
	}

	// Merge system prompt according to promptMode / explicit config
	var finalMsgs []map[string]any
	sysText := strings.Join(systemParts, "\n\n")
	if meta.PromptMode == "rewrite" && meta.SystemPrompt != "" {
		sysText = meta.SystemPrompt
	} else if meta.PromptMode != "passthrough" && meta.SystemPrompt != "" {
		if sysText != "" {
			sysText = meta.SystemPrompt + "\n\n" + sysText
		} else {
			sysText = meta.SystemPrompt
		}
	} else if sysText == "" && meta.PromptMode != "passthrough" && meta.PromptMode != "" {
		if eff, perr := prompt.Load(meta.PromptMode, ""); perr == nil && eff != "" {
			sysText = eff
		}
	}
	if sysText != "" {
		finalMsgs = append(finalMsgs, map[string]any{"role": "system", "content": sysText})
	}
	finalMsgs = append(finalMsgs, chatMsgs...)
	finalMsgs = RepairToolPairing(finalMsgs)
	finalMsgs = BackfillReasoningContent(finalMsgs, anyStr(req["model"]), modelKey)

	tools := NormalizeTools(req["tools"])
	toolChoice := NormalizeToolChoice(req["tool_choice"])
	finalMsgs = ApplyKimiToolProtocol(finalMsgs, anyStr(req["model"]), modelKey, len(tools) > 0, toolChoice)

	// Convert to []any for JSON serialization
	msgInterfaces := make([]any, len(finalMsgs))
	for i, m := range finalMsgs {
		msgInterfaces[i] = m
	}
	body["messages"] = msgInterfaces
	if tools != nil {
		body["tools"] = tools
	} else {
		body["tools"] = []any{}
	}
	if toolChoice != nil {
		body["tool_choice"] = toolChoice
	}

	nid := cosy.NewUUID()
	body["request_id"] = nid
	if meta.ConversationID != "" {
		body["chat_record_id"] = meta.ConversationID
	} else {
		body["chat_record_id"] = nid
	}
	body["request_set_id"] = cosy.NewUUID()
	if meta.SessionID != "" {
		body["session_id"] = meta.SessionID
	} else {
		body["session_id"] = cosy.NewUUID()
	}
	body["stream"] = true
	body["agent_id"] = "agent_common"

	userType := "personal_standard"
	if a != nil && a.UserTypeValue() != "" {
		userType = a.UserTypeValue()
	}
	body["aliyun_user_type"] = userType

	// model_config from live/static catalog
	catMeta, _ := CatalogMetaFor(modelKey, realm)
	mc, _ := body["model_config"].(map[string]any)
	if mc == nil {
		mc = make(map[string]any)
	}
	mc["key"] = modelKey
	if catMeta.DisplayName != "" {
		mc["display_name"] = catMeta.DisplayName
	} else {
		mc["display_name"] = modelKey
	}
	mc["model"] = ""
	mc["format"] = "openai"
	mc["is_vl"] = catMeta.IsVL
	mc["is_reasoning"] = catMeta.IsReasoning
	mc["api_key"] = ""
	mc["url"] = ""
	mc["source"] = "system"
	mc["max_input_tokens"] = MaxContextLength(catMeta)
	body["model_config"] = mc

	// chat_context
	cc, _ := body["chat_context"].(map[string]any)
	if cc == nil {
		cc = make(map[string]any)
	}
	cc["text"] = map[string]any{"type": "text", "text": latestPrompt}
	extra, _ := cc["extra"].(map[string]any)
	if extra == nil {
		extra = make(map[string]any)
	}
	extra["originalContent"] = map[string]any{"type": "text", "text": latestPrompt}
	extra["modelConfig"] = mc
	cc["extra"] = extra
	body["chat_context"] = cc

	// parameters: max_tokens + normalized reasoning_effort + temperature/top_p
	params, _ := body["parameters"].(map[string]any)
	if params == nil {
		params = make(map[string]any)
	}
	maxTok := anyInt(req["max_tokens"])
	if maxTok <= 0 {
		maxTok = anyInt(req["max_completion_tokens"])
	}
	if maxTok > 0 {
		params["max_tokens"] = maxTok
	}
	if eff := RequestedEffort(req); eff != "" {
		norm, _ := NormalizeReasoningEffort(eff, catMeta)
		if norm != "" {
			params["reasoning_effort"] = norm
		}
	}
	if t, ok := toFloat(req["temperature"]); ok {
		params["temperature"] = t
	}
	if tp, ok := toFloat(req["top_p"]); ok {
		params["top_p"] = tp
	}
	body["parameters"] = params

	// business attribution card (model_key ensures Qoder bills properly instead of falling back to auto)
	biz, _ := body["business"].(map[string]any)
	if biz == nil {
		biz = make(map[string]any)
	}
	if meta.ConversationID != "" {
		biz["id"] = meta.ConversationID
	} else {
		biz["id"] = nid
	}
	if meta.BeginAtMs > 0 {
		biz["begin_at"] = meta.BeginAtMs
	} else {
		biz["begin_at"] = time.Now().UnixMilli()
	}
	promptRunes := []rune(latestPrompt)
	if len(promptRunes) > 30 {
		promptRunes = promptRunes[:30]
	}
	if len(promptRunes) > 0 {
		biz["name"] = string(promptRunes)
	} else {
		biz["name"] = "chat"
	}
	biz["product"] = "cli"
	biz["type"] = "agent"
	biz["stage"] = "start"
	biz["model_key"] = modelKey
	biz["version"] = cosy.CosyVersion()
	body["business"] = biz

	// Verify serialized size stays within Aliyun SLB 30MB bound before base64 encoding.
	rawCheck, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	estEncoded := 4 * ((len(rawCheck) + 2) / 3)
	if estEncoded > MaxOutboundBodyBytes {
		return nil, &Error{
			Kind:   ErrPromptTooLong,
			Status: 413,
			Msg:    fmt.Sprintf("outbound payload exceeds %d bytes (%d)", MaxOutboundBodyBytes, estEncoded),
		}
	}
	return body, nil
}
