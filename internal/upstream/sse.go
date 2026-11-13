package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError reports whether upstream closed the stream cleanly with no valid frames.
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// StreamStats records per-stream response metrics observed during SSE relay.
type StreamStats struct {
	FirstByteAt      time.Time
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	ErrorFrame       string
	FinishReason     string
	Frames           int
}

// QoderSSEEnvelope wraps Qoder's upstream wire SSE frame format:
//
//	data:{"headers":{...},"body":"<inner OpenAI JSON>","statusCodeValue":200}
//	data:{"body":"[DONE]"}
type qoderSSEEnvelope struct {
	Body            any    `json:"body"`
	StatusCodeValue any    `json:"statusCodeValue"`
	StatusCode      string `json:"statusCode,omitempty"`
}

// unwrapQoderReader unwraps Qoder's {"headers","body","statusCodeValue"} envelope stream
// into standard OpenAI "data: <inner JSON>\n\n" lines.
// Peeks until the first valid inner chunk (or [DONE] / error) before returning so HTTP 200
// streams carrying wrapped statusCodeValue=418/403/10605 trigger clean upstream retries/failover.
func unwrapQoderReader(raw io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(raw, 64*1024)
	var prefetched []string

	readNextNormalized := func() (frame string, done bool, err error) {
		for {
			line, rerr := br.ReadString('\n')
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, ":") {
				if rerr != nil {
					return "", true, rerr
				}
				continue
			}
			// Ignore event:finish{...} telemetry lines emitted by Qoder gateway.
			if strings.HasPrefix(trimmed, "event:finish") || strings.HasPrefix(trimmed, "event: finish") {
				if rerr != nil {
					return "", true, rerr
				}
				continue
			}
			if !strings.HasPrefix(trimmed, "data:") {
				if rerr != nil {
					return "", true, rerr
				}
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" {
				if rerr != nil {
					return "", true, rerr
				}
				continue
			}
			if payload == "[DONE]" {
				return "data: [DONE]\n\n", true, nil
			}
			var env qoderSSEEnvelope
			if json.Unmarshal([]byte(payload), &env) != nil {
				// Not an envelope (e.g. mock direct OpenAI SSE): pass through untouched.
				return "data: " + payload + "\n\n", false, nil
			}
			// Non-envelope raw OpenAI chunk (no body field & has choices/error)
			if env.Body == nil && env.StatusCodeValue == nil {
				return "data: " + payload + "\n\n", false, nil
			}

			st := parseStatusCodeValue(env.StatusCodeValue)
			bodyStr := anyStr(env.Body)
			if bodyStr == "" && env.Body != nil {
				if b, merr := json.Marshal(env.Body); merr == nil {
					bodyStr = string(b)
				}
			}
			if st != 0 && st != http.StatusOK {
				return "", false, ClassifyError(st, bodyStr)
			}
			if bodyStr == "" {
				if rerr != nil {
					return "", true, rerr
				}
				continue
			}
			if bodyStr == "[DONE]" {
				return "data: [DONE]\n\n", true, nil
			}
			// Inspect inner JSON for embedded upstream errors before yielding.
			var inner map[string]any
			if json.Unmarshal([]byte(bodyStr), &inner) == nil {
				if eObj, hasErr := inner["error"]; hasErr && eObj != nil {
					return "", false, ClassifyError(502, bodyStr)
				}
			}
			return "data: " + bodyStr + "\n\n", false, nil
		}
	}

	// Synchronously peek first meaningful frame so wrapped 418/403/10605/TOKEN_EXPIRE trigger failover.
	for {
		frame, done, err := readNextNormalized()
		if err != nil && !errors.Is(err, io.EOF) {
			_ = raw.Close()
			return nil, err
		}
		if frame != "" {
			prefetched = append(prefetched, frame)
		}
		if done || len(prefetched) > 0 || errors.Is(err, io.EOF) {
			break
		}
	}

	if len(prefetched) == 0 {
		_ = raw.Close()
		return nil, errEmptyStream
	}

	pr, pw := io.Pipe()
	go func() {
		defer raw.Close()
		defer pw.Close()
		for _, pf := range prefetched {
			if _, werr := io.WriteString(pw, pf); werr != nil {
				return
			}
			if pf == "data: [DONE]\n\n" {
				return
			}
		}
		for {
			frame, done, err := readNextNormalized()
			if err != nil && !errors.Is(err, io.EOF) {
				errPayload, _ := json.Marshal(map[string]any{
					"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
				})
				_, _ = io.WriteString(pw, "data: "+string(errPayload)+"\n\n")
				_, _ = io.WriteString(pw, "data: [DONE]\n\n")
				return
			}
			if frame != "" {
				if _, werr := io.WriteString(pw, frame); werr != nil {
					return
				}
			}
			if done || errors.Is(err, io.EOF) {
				if frame != "data: [DONE]\n\n" {
					_, _ = io.WriteString(pw, "data: [DONE]\n\n")
				}
				return
			}
		}
	}()
	return pr, nil
}

func parseStatusCodeValue(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n
	default:
		return 0
	}
}

// StreamHint relays standard OpenAI SSE frames from r to w while recording token stats
// and injecting optional gateway_hint metadata onto error frames.
func StreamHint(w http.ResponseWriter, r io.Reader, stats *StreamStats, hintFn func(string) string) error {
	br := bufio.NewReaderSize(r, 64*1024)
	flusher, _ := w.(http.Flusher)
	validEvents := 0
	sawDone := false

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			// Forward SSE keepalive ping comments cleanly
			if _, werr := io.WriteString(w, trimmed+"\n\n"); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if err != nil {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				sawDone = true
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				break
			}
			if payload != "" {
				validEvents++
				if stats != nil {
					stats.Frames++
					if stats.FirstByteAt.IsZero() {
						stats.FirstByteAt = time.Now()
					}
				}
				outPayload := payload
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					extractStatsFromChunk(chunk, stats)
					if errObj, hasErr := chunk["error"].(map[string]any); hasErr && errObj != nil {
						if stats != nil && stats.ErrorFrame == "" {
							stats.ErrorFrame = payload
						}
						if hintFn != nil {
							if hint := hintFn(payload); hint != "" {
								errObj["gateway_hint"] = hint
								if b, merr := json.Marshal(chunk); merr == nil {
									outPayload = string(b)
								}
							}
						}
					}
				}
				if _, werr := fmt.Fprintf(w, "data: %s\n\n", outPayload); werr != nil {
					return werr
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	if !sawDone && validEvents > 0 {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if validEvents == 0 {
		return errEmptyStream
	}
	return nil
}

func extractStatsFromChunk(chunk map[string]any, stats *StreamStats) {
	if stats == nil || chunk == nil {
		return
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		if pt, ok := toFloat(usage["prompt_tokens"]); ok {
			stats.PromptTokens = int64(pt)
		}
		if ct, ok := toFloat(usage["completion_tokens"]); ok {
			stats.CompletionTokens = int64(ct)
		}
		if tt, ok := toFloat(usage["total_tokens"]); ok {
			stats.TotalTokens = int64(tt)
		}
		if det, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			if cached, ok := toFloat(det["cached_tokens"]); ok {
				stats.CachedTokens = int64(cached)
			}
		}
		if stats.CachedTokens == 0 {
			if cached, ok := toFloat(usage["cached_tokens"]); ok {
				stats.CachedTokens = int64(cached)
			}
		}
	}
	if choices, ok := chunk["choices"].([]any); ok {
		for _, ch := range choices {
			cm, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			if fr := strings.TrimSpace(anyStr(cm["finish_reason"])); fr != "" {
				stats.FinishReason = fr
			}
		}
	}
}

// Aggregate folds a complete OpenAI SSE stream into a single non-streaming chat.completion object.
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model    string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		gotContent   bool
		validEvents  int
		toolCalls    = map[int]map[string]any{}
		idIndex      = map[string]int{}
		toolSeq      int
	)

	nextToolIdx := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCalls := func(tcs []any) {
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			} else if cid := anyStr(tc["id"]); cid != "" {
				if prev, seen := idIndex[cid]; seen {
					idx = prev
				} else {
					idx = nextToolIdx()
					idIndex[cid] = idx
				}
			} else if len(toolCalls) > 0 {
				// Append continuation delta fragments without index to most recent tool call
				maxIdx := -1
				for k := range toolCalls {
					if k > maxIdx {
						maxIdx = k
					}
				}
				idx = maxIdx
			} else {
				idx = nextToolIdx()
			}
			slot, exists := toolCalls[idx]
			if !exists {
				slot = map[string]any{
					"id":       anyStr(tc["id"]),
					"type":     "function",
					"function": map[string]any{"name": "", "arguments": ""},
				}
				toolCalls[idx] = slot
			}
			if cid := anyStr(tc["id"]); cid != "" {
				slot["id"] = cid
				idIndex[cid] = idx
			}
			if tp := anyStr(tc["type"]); tp != "" {
				slot["type"] = tp
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				dstFn := slot["function"].(map[string]any)
				if name := anyStr(fn["name"]); name != "" {
					dstFn["name"] = dstFn["name"].(string) + name
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					dstFn["arguments"] = dstFn["arguments"].(string) + args
				}
			}
		}
	}

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			var chunk map[string]any
			if payload != "" && json.Unmarshal([]byte(payload), &chunk) == nil {
				validEvents++
				if errObj, hasErr := chunk["error"]; hasErr && errObj != nil {
					return nil, ClassifyError(502, payload)
				}
				if v := anyStr(chunk["id"]); v != "" {
					id = v
				}
				if v := anyStr(chunk["model"]); v != "" {
					model = v
				}
				if v, ok := toFloat(chunk["created"]); ok && v > 0 {
					created = v
				}
				if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
					usage = u
				}
				if choices, ok := chunk["choices"].([]any); ok {
					for _, rawC := range choices {
						ch, ok := rawC.(map[string]any)
						if !ok {
							continue
						}
						if fr := anyStr(ch["finish_reason"]); fr != "" {
							finishReason = fr
						}
						if delta, ok := ch["delta"].(map[string]any); ok {
							if r := anyStr(delta["role"]); r != "" {
								role = r
							}
							if c, ok := delta["content"].(string); ok && c != "" {
								content.WriteString(c)
								gotContent = true
							}
							if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
								reasoning.WriteString(rc)
							} else if r, ok := delta["reasoning"].(string); ok && r != "" {
								reasoning.WriteString(r)
							}
							if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
								mergeToolCalls(tcs)
							}
						}
						if msg, ok := ch["message"].(map[string]any); ok && !gotContent {
							if c, ok := msg["content"].(string); ok && c != "" {
								content.WriteString(c)
								gotContent = true
							}
							if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
								mergeToolCalls(tcs)
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if validEvents == 0 {
		return nil, errEmptyStream
	}
	if id == "" {
		id = "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}

	txt := content.String()
	// Recover leaked text tool calls when model echoes Qoder's historical marker instead of native tool_calls.
	var orderedTools []any
	if len(toolCalls) == 0 && strings.Contains(txt, "请求调用工具") {
		if recovered, cleaned := recoverLeakedToolCalls(txt); len(recovered) > 0 {
			orderedTools = recovered
			txt = cleaned
			finishReason = "tool_calls"
		}
	} else if len(toolCalls) > 0 {
		keys := make([]int, 0, len(toolCalls))
		for k := range toolCalls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			orderedTools = append(orderedTools, toolCalls[k])
		}
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	msg := map[string]any{
		"role":    role,
		"content": txt,
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(orderedTools) > 0 {
		msg["tool_calls"] = orderedTools
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

var leakedMarkerRe = regexp.MustCompile(`(?s)(?:\[assistant 请求调用工具\]|<assistant 请求调用工具>)\s*(\[.*\])`)

func recoverLeakedToolCalls(txt string) ([]any, string) {
	m := leakedMarkerRe.FindStringSubmatchIndex(txt)
	if len(m) < 4 {
		return nil, txt
	}
	jsonPart := txt[m[2]:m[3]]
	var arr []map[string]any
	if json.Unmarshal([]byte(jsonPart), &arr) != nil || len(arr) == 0 {
		return nil, txt
	}
	out := make([]any, 0, len(arr))
	for i, item := range arr {
		name := anyStr(item["name"])
		if name == "" {
			continue
		}
		args := anyStr(item["arguments"])
		if args == "" && item["arguments"] != nil {
			if b, err := json.Marshal(item["arguments"]); err == nil {
				args = string(b)
			}
		}
		if args == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id":   fmt.Sprintf("call_recovered_%d", i),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		})
	}
	if len(out) == 0 {
		return nil, txt
	}
	cleaned := strings.TrimSpace(txt[:m[0]] + txt[m[1]:])
	return out, cleaned
}
