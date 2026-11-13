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
)

// responsesAPI implements OpenAI Responses API (/v1/responses) for Codex CLI compatibility:
// supports instructions, string/array input items (including function_call & function_call_output),
// namespace tool flattening, reasoning.effort, and streaming/non-streaming responses.
func (h *Handler) responsesAPI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req, err := decodeJSONMap(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = "auto"
	}
	stream, _ := req["stream"].(bool)
	instructions, _ := req["instructions"].(string)

	msgs := responsesInputToMessages(req["input"], instructions)
	openAIReq := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   stream,
	}
	if mt := req["max_output_tokens"]; mt != nil {
		openAIReq["max_tokens"] = mt
	} else if mt := req["max_tokens"]; mt != nil {
		openAIReq["max_tokens"] = mt
	}
	if temp := req["temperature"]; temp != nil {
		openAIReq["temperature"] = temp
	}
	if reasoning, ok := req["reasoning"].(map[string]any); ok {
		openAIReq["reasoning"] = reasoning
		if eff, _ := reasoning["effort"].(string); eff != "" {
			openAIReq["reasoning_effort"] = eff
		}
	}
	if tools := flattenResponsesTools(req["tools"]); len(tools) > 0 {
		openAIReq["tools"] = tools
	}
	if tc := req["tool_choice"]; tc != nil {
		openAIReq["tool_choice"] = tc
	}
	if md := req["metadata"]; md != nil {
		openAIReq["metadata"] = md
	}

	reqBytes, _ := json.Marshal(openAIReq)
	newReq := r.Clone(r.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(reqBytes))
	newReq.ContentLength = int64(len(reqBytes))

	respID := "resp_" + randomHex(12)

	if !stream {
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
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "invalid completion response")
			return
		}
		writeJSON(w, http.StatusOK, buildResponsesPayload(respID, model, oResp))
		return
	}

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

	writeEvt := func(evt string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt, string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgItemID := "msg_" + randomHex(10)
	started := false
	textStarted := false
	nextOutIdx := 0
	textOutIdx := 0
	var textBuf strings.Builder
	toolItems := make(map[int]*codexToolItem)
	var usage map[string]any

	ensureCreated := func() {
		if started {
			return
		}
		started = true
		writeEvt("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": respID, "object": "response", "model": model, "status": "in_progress", "output": []any{},
			},
		})
	}
	ensureTextItem := func() {
		ensureCreated()
		if textStarted {
			return
		}
		textStarted = true
		textOutIdx = nextOutIdx
		nextOutIdx++
		writeEvt("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": textOutIdx,
			"item": map[string]any{
				"id": msgItemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{},
			},
		})
		writeEvt("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       msgItemID,
			"output_index":  textOutIdx,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		})
	}

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

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
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": errObj})
				return
			}
			writeEvt("response.failed", map[string]any{"type": "response.failed", "error": errObj})
			return
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}

		if txt, _ := delta["content"].(string); txt != "" {
			ensureTextItem()
			textBuf.WriteString(txt)
			writeEvt("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgItemID,
				"output_index":  textOutIdx,
				"content_index": 0,
				"delta":         txt,
			})
		}

		if tcs, ok := delta["tool_calls"].([]any); ok {
			ensureCreated()
			for _, rawTC := range tcs {
				tc, _ := rawTC.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if f, ok := tc["index"].(float64); ok {
					idx = int(f)
				}
				ti, exists := toolItems[idx]
				fn, _ := tc["function"].(map[string]any)
				if !exists {
					cid, _ := tc["id"].(string)
					if cid == "" {
						cid = fmt.Sprintf("call_%s", randomHex(6))
					}
					name := ""
					if fn != nil {
						name, _ = fn["name"].(string)
					}
					ti = &codexToolItem{
						itemID:   "fc_" + randomHex(8),
						callID:   cid,
						name:     name,
						outIndex: nextOutIdx,
					}
					nextOutIdx++
					toolItems[idx] = ti
					writeEvt("response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": ti.outIndex,
						"item": map[string]any{
							"id":        ti.itemID,
							"type":      "function_call",
							"call_id":   ti.callID,
							"name":      ti.name,
							"arguments": "",
							"status":    "in_progress",
						},
					})
				}
				if fn != nil {
					if part, _ := fn["arguments"].(string); part != "" {
						ti.args.WriteString(part)
						writeEvt("response.function_call_arguments.delta", map[string]any{
							"type":         "response.function_call_arguments.delta",
							"item_id":      ti.itemID,
							"output_index": ti.outIndex,
							"delta":        part,
						})
					}
				}
			}
		}
	}

	if !started {
		writeOpenAIError(w, http.StatusBadGateway, "empty_stream", "empty upstream stream")
		return
	}

	var output []any
	if textStarted {
		fullText := textBuf.String()
		writeEvt("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgItemID, "output_index": textOutIdx, "content_index": 0, "text": fullText,
		})
		msgDone := map[string]any{
			"id": msgItemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": fullText}},
		}
		writeEvt("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": textOutIdx, "item": msgDone,
		})
		output = append(output, msgDone)
	}
	for _, ti := range toolItems {
		argsStr := ti.args.String()
		if argsStr == "" {
			argsStr = "{}"
		}
		fcDone := map[string]any{
			"id":        ti.itemID,
			"type":      "function_call",
			"call_id":   ti.callID,
			"name":      ti.name,
			"arguments": argsStr,
			"status":    "completed",
		}
		writeEvt("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": ti.itemID, "output_index": ti.outIndex, "arguments": argsStr,
		})
		writeEvt("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": ti.outIndex, "item": fcDone,
		})
		output = append(output, fcDone)
	}

	completedResp := map[string]any{
		"id": respID, "object": "response", "model": model, "status": "completed", "output": output,
	}
	if usage != nil {
		completedResp["usage"] = usageToResponsesUsage(usage)
	}
	writeEvt("response.completed", map[string]any{"type": "response.completed", "response": completedResp})
}

type codexToolItem struct {
	itemID   string
	callID   string
	name     string
	outIndex int
	args     strings.Builder
}

func responsesInputToMessages(rawInput any, instructions string) []any {
	var msgs []any
	if strings.TrimSpace(instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	switch v := rawInput.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
			switch typ {
			case "function_call":
				cid, _ := m["call_id"].(string)
				if cid == "" {
					cid, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				if args == "" {
					args = "{}"
				}
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{map[string]any{
						"id":       cid,
						"type":     "function",
						"function": map[string]any{"name": name, "arguments": args},
					}},
				})
			case "function_call_output":
				cid, _ := m["call_id"].(string)
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": cid,
					"content":      flattenToolResultContent(m["output"]),
				})
			default:
				role := fmt.Sprint(m["role"])
				if role == "" || role == "<nil>" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{
					"role":    role,
					"content": normalizeResponsesContent(m["content"]),
				})
			}
		}
	}
	return msgs
}

func normalizeResponsesContent(raw any) any {
	switch c := raw.(type) {
	case string:
		return c
	case []any:
		var out []any
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pt := strings.ToLower(fmt.Sprint(pm["type"]))
			switch pt {
			case "input_text", "output_text", "text":
				if txt, _ := pm["text"].(string); txt != "" {
					out = append(out, map[string]any{"type": "text", "text": txt})
				}
			case "input_image":
				if iu, _ := pm["image_url"].(string); iu != "" {
					out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": iu}})
				} else if ium, ok := pm["image_url"].(map[string]any); ok {
					out = append(out, map[string]any{"type": "image_url", "image_url": ium})
				}
			default:
				out = append(out, pm)
			}
		}
		if len(out) == 1 {
			if m, ok := out[0].(map[string]any); ok && m["type"] == "text" {
				return m["text"]
			}
		}
		return out
	default:
		return ""
	}
}

// flattenResponsesTools flattens Codex Responses API "namespace" tool groups into individual functions
// alongside regular top-level or nested function tools.
func flattenResponsesTools(raw any) []any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
		if typ == "namespace" {
			nsName, _ := m["name"].(string)
			subTools, _ := m["tools"].([]any)
			for _, subRaw := range subTools {
				sub, ok := subRaw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := sub["name"].(string)
				if nsName != "" && !strings.Contains(name, ".") && !strings.Contains(name, "__") {
					name = nsName + "__" + name
				}
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": sub["description"],
						"parameters":  sub["parameters"],
					},
				})
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

func buildResponsesPayload(respID, model string, oResp map[string]any) map[string]any {
	var output []any
	if choices, ok := oResp["choices"].([]any); ok && len(choices) > 0 {
		ch, _ := choices[0].(map[string]any)
		if msg, ok := ch["message"].(map[string]any); ok {
			if txt, _ := msg["content"].(string); txt != "" {
				output = append(output, map[string]any{
					"id": "msg_" + randomHex(10), "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": txt}},
				})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, rawTC := range tcs {
					tc, _ := rawTC.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					output = append(output, map[string]any{
						"id":        "fc_" + randomHex(8),
						"type":      "function_call",
						"call_id":   tc["id"],
						"name":      fn["name"],
						"arguments": fn["arguments"],
						"status":    "completed",
					})
				}
			}
		}
	}
	res := map[string]any{
		"id": respID, "object": "response", "model": model, "status": "completed", "output": output,
	}
	if u, ok := oResp["usage"].(map[string]any); ok {
		res["usage"] = usageToResponsesUsage(u)
	}
	return res
}

func usageToResponsesUsage(u map[string]any) map[string]any {
	return map[string]any{
		"input_tokens":  u["prompt_tokens"],
		"output_tokens": u["completion_tokens"],
		"total_tokens":  u["total_tokens"],
	}
}

func encodeJSONBytes(v any) ([]byte, error) {
	return json.Marshal(v)
}

func decodeJSONMap(b []byte) (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(b, &m)
	return m, err
}
