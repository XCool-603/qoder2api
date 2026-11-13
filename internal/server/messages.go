package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"qoder2api/internal/upstream"
)

type anthropicMessageReq struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Messages      []map[string]any `json:"messages"`
	System        any              `json:"system,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Tools         []map[string]any `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
	Thinking      map[string]any   `json:"thinking,omitempty"`
	Metadata      map[string]any   `json:"metadata,omitempty"`
}

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicMessageReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = "smodel"
	}

	openAIReq := convertAnthropicToOpenAI(&req)
	reqBytes, err := json.Marshal(openAIReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	newReq := r.Clone(r.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(reqBytes))
	newReq.ContentLength = int64(len(reqBytes))
	msgID := "msg_" + randomHex(12)

	if !req.Stream {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, newReq)
		if rec.Code >= 400 {
			for k, vv := range rec.Header() {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		var oResp map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &oResp) != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid internal completion payload")
			return
		}
		claudeResp := openAIResponseToAnthropic(msgID, req.Model, oResp)
		writeJSON(w, http.StatusOK, claudeResp)
		return
	}

	// Streaming Anthropic SSE bridge
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		cw := &pipeResponseWriter{pipeWriter: pw, header: make(http.Header), code: http.StatusOK}
		h.chatCompletions(cw, newReq)
	}()

	writeEvt := func(evt string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt, string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	started := false
	nextBlockIdx := 0
	textBlockIdx := -1
	thinkBlockIdx := -1
	toolBlockMap := make(map[int]int) // openai tool_call index -> anthropic content_block index
	stopReason := "end_turn"
	var inTok, outTok int

	ensureStart := func() {
		if started {
			return
		}
		started = true
		writeEvt("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"model":         req.Model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": inTok, "output_tokens": 0},
			},
		})
	}
	closeOpenText := func() {
		if textBlockIdx >= 0 {
			writeEvt("content_block_stop", map[string]any{"type": "content_block_stop", "index": textBlockIdx})
			textBlockIdx = -1
		}
	}
	closeOpenThink := func() {
		if thinkBlockIdx >= 0 {
			writeEvt("content_block_stop", map[string]any{"type": "content_block_stop", "index": thinkBlockIdx})
			thinkBlockIdx = -1
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ":") {
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if errObj, hasErr := chunk["error"].(map[string]any); hasErr && errObj != nil {
			if !started {
				writeJSON(w, http.StatusBadGateway, map[string]any{"type": "error", "error": errObj})
				return
			}
			writeEvt("error", map[string]any{"type": "error", "error": errObj})
			return
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			if pt, ok := u["prompt_tokens"].(float64); ok && pt > 0 {
				inTok = int(pt)
			}
			if ct, ok := u["completion_tokens"].(float64); ok && ct > 0 {
				outTok = int(ct)
			}
		}

		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls", "function_call":
				stopReason = "tool_use"
			default:
				stopReason = "end_turn"
			}
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}

		// Thinking deltas
		var rc string
		if s, _ := delta["reasoning_content"].(string); s != "" {
			rc = s
		} else if s, _ := delta["reasoning"].(string); s != "" {
			rc = s
		}
		if rc != "" && req.Thinking != nil && strings.EqualFold(fmt.Sprint(req.Thinking["type"]), "enabled") {
			ensureStart()
			if thinkBlockIdx < 0 {
				thinkBlockIdx = nextBlockIdx
				nextBlockIdx++
				writeEvt("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         thinkBlockIdx,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				})
			}
			writeEvt("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": thinkBlockIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
			})
		}

		// Text deltas
		if txt, _ := delta["content"].(string); txt != "" {
			ensureStart()
			closeOpenThink()
			if textBlockIdx < 0 {
				textBlockIdx = nextBlockIdx
				nextBlockIdx++
				writeEvt("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         textBlockIdx,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
			}
			outTok++
			writeEvt("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textBlockIdx,
				"delta": map[string]any{"type": "text_delta", "text": txt},
			})
		}

		// Tool calls deltas
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			ensureStart()
			closeOpenThink()
			closeOpenText()
			stopReason = "tool_use"
			for _, rawTC := range tcs {
				tc, _ := rawTC.(map[string]any)
				if tc == nil {
					continue
				}
				tcIdx := 0
				if f, ok := tc["index"].(float64); ok {
					tcIdx = int(f)
				}
				blockIdx, exists := toolBlockMap[tcIdx]
				fn, _ := tc["function"].(map[string]any)
				if !exists {
					blockIdx = nextBlockIdx
					nextBlockIdx++
					toolBlockMap[tcIdx] = blockIdx
					id, _ := tc["id"].(string)
					if id == "" {
						id = fmt.Sprintf("toolu_%d_%s", tcIdx, randomHex(4))
					}
					name := ""
					if fn != nil {
						name, _ = fn["name"].(string)
					}
					writeEvt("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIdx,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": map[string]any{},
						},
					})
				}
				if fn != nil {
					if partial, _ := fn["arguments"].(string); partial != "" {
						outTok++
						writeEvt("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": blockIdx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
						})
					}
				}
			}
		}
	}

	if !started {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "empty upstream stream")
		return
	}
	closeOpenThink()
	closeOpenText()
	for _, bIdx := range toolBlockMap {
		writeEvt("content_block_stop", map[string]any{"type": "content_block_stop", "index": bIdx})
	}
	writeEvt("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTok},
	})
	writeEvt("message_stop", map[string]any{"type": "message_stop"})
}

func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req anthropicMessageReq
	if json.Unmarshal(body, &req) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	openAIReq := convertAnthropicToOpenAI(&req)
	raw, _ := json.Marshal(openAIReq)
	// Fast UTF-8 character heuristic (~3.3 chars/token + overhead per message/tool)
	est := len(raw)/4 + len(req.Tools)*30 + 12
	if est < 1 {
		est = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": est})
}

func convertAnthropicToOpenAI(req *anthropicMessageReq) map[string]any {
	var msgs []any
	// 1. System prompt
	if sysStr := extractAnthropicSystemText(req.System); sysStr != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sysStr})
	}

	// 2. Conversation turns supporting text, image, tool_use, tool_result, thinking
	for _, m := range req.Messages {
		role := fmt.Sprint(m["role"])
		rawContent := m["content"]
		switch content := rawContent.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		case []any:
			var textParts []any
			var toolCalls []any
			var toolResults []map[string]any
			var thinkStr strings.Builder

			for _, item := range content {
				bk, ok := item.(map[string]any)
				if !ok {
					continue
				}
				switch strings.ToLower(fmt.Sprint(bk["type"])) {
				case "text":
					if t, _ := bk["text"].(string); t != "" {
						textParts = append(textParts, map[string]any{"type": "text", "text": t})
					}
				case "thinking":
					if t, _ := bk["thinking"].(string); t != "" {
						thinkStr.WriteString(t)
					}
				case "image":
					if src, ok := bk["source"].(map[string]any); ok {
						mt, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						if mt != "" && data != "" {
							textParts = append(textParts, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
							})
						} else if u, _ := src["url"].(string); u != "" {
							textParts = append(textParts, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": u},
							})
						}
					}
				case "tool_use":
					argsBytes, _ := json.Marshal(bk["input"])
					if len(argsBytes) == 0 || string(argsBytes) == "null" {
						argsBytes = []byte("{}")
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   fmt.Sprint(bk["id"]),
						"type": "function",
						"function": map[string]any{
							"name":      fmt.Sprint(bk["name"]),
							"arguments": string(argsBytes),
						},
					})
				case "tool_result":
					outTxt := flattenToolResultContent(bk["content"])
					toolResults = append(toolResults, map[string]any{
						"role":         "tool",
						"tool_call_id": fmt.Sprint(bk["tool_use_id"]),
						"content":      outTxt,
					})
				}
			}

			if len(toolResults) > 0 {
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
				}
				if len(textParts) > 0 {
					msgs = append(msgs, map[string]any{"role": role, "content": textParts})
				}
				continue
			}

			msgObj := map[string]any{"role": role}
			if len(textParts) == 1 && fmt.Sprint(textParts[0].(map[string]any)["type"]) == "text" {
				msgObj["content"] = textParts[0].(map[string]any)["text"]
			} else if len(textParts) > 0 {
				msgObj["content"] = textParts
			} else {
				msgObj["content"] = ""
			}
			if thinkStr.Len() > 0 {
				msgObj["reasoning_content"] = thinkStr.String()
			}
			if len(toolCalls) > 0 {
				msgObj["tool_calls"] = toolCalls
			}
			msgs = append(msgs, msgObj)
		}
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		var openTools []any
		for _, t := range req.Tools {
			openTools = append(openTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t["name"],
					"description": t["description"],
					"parameters":  t["input_schema"],
				},
			})
		}
		out["tools"] = openTools
	}
	if req.ToolChoice != nil {
		out["tool_choice"] = upstream.NormalizeToolChoice(req.ToolChoice)
	}
	if req.Thinking != nil {
		out["thinking"] = req.Thinking
		if eff := upstream.RequestedEffort(out); eff != "" {
			out["reasoning_effort"] = eff
		}
	}
	if req.Metadata != nil {
		out["metadata"] = req.Metadata
	}
	return out
}

func openAIResponseToAnthropic(msgID, reqModel string, oResp map[string]any) map[string]any {
	var blocks []any
	stopReason := "end_turn"
	if choices, ok := oResp["choices"].([]any); ok && len(choices) > 0 {
		ch, _ := choices[0].(map[string]any)
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls", "function_call":
				stopReason = "tool_use"
			}
		}
		if msg, ok := ch["message"].(map[string]any); ok {
			if rc, _ := msg["reasoning_content"].(string); rc != "" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": rc})
			}
			if txt, _ := msg["content"].(string); txt != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": txt})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, rawTC := range tcs {
					tc, _ := rawTC.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					var inputObj map[string]any
					if argsStr, _ := fn["arguments"].(string); argsStr != "" {
						_ = json.Unmarshal([]byte(argsStr), &inputObj)
					}
					if inputObj == nil {
						inputObj = map[string]any{}
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    tc["id"],
						"name":  fn["name"],
						"input": inputObj,
					})
				}
				if len(tcs) > 0 {
					stopReason = "tool_use"
				}
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	var inTok, outTok int
	if u, ok := oResp["usage"].(map[string]any); ok {
		if pt, ok := u["prompt_tokens"].(float64); ok {
			inTok = int(pt)
		}
		if ct, ok := u["completion_tokens"].(float64); ok {
			outTok = int(ct)
		}
	}
	return map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       blocks,
		"model":         reqModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	}
}

func extractAnthropicSystemText(sys any) string {
	switch v := sys.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(t)
				}
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

func flattenToolResultContent(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if v == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

type pipeResponseWriter struct {
	pipeWriter *io.PipeWriter
	header     http.Header
	code       int
}

func (p *pipeResponseWriter) Header() http.Header         { return p.header }
func (p *pipeResponseWriter) WriteHeader(statusCode int)  { p.code = statusCode }
func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.pipeWriter.Write(b) }
func (p *pipeResponseWriter) Flush()                      {}
