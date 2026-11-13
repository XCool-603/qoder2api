package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
	"qoder2api/internal/usermgr"
)

const sseOK = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		if a != nil {
			p.Add(a)
		}
	}
	return p
}

func testPoolWithIDs(uids ...string) *pool.Pool {
	p := pool.New("")
	for _, uid := range uids {
		a := &auth.Auth{
			UID:         uid,
			AccessToken: "dt-" + uid,
			Domain:      "qoder.com",
			ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		}
		a.SetRealm("global")
		p.Add(a)
	}
	return p
}

func newFakeUpstream(t *testing.T, fn func(authz string) (int, string, bool)) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		switch r.Header.Get("cosy-user") {
		case "bad":
			authz = "Bearer at-bad"
		case "good":
			authz = "Bearer at-good"
		case "u1":
			authz = "Bearer at1"
		case "u2":
			authz = "Bearer at2"
		case "u3":
			authz = "Bearer at3"
		}
		status, body, isSSE := fn(authz)
		if isSSE {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.HTTP = srv.Client()
	up.APIBaseCN = srv.URL
	up.APIBaseGlobal = srv.URL
	up.OpenAPICN = srv.URL
	up.OpenAPIGlobal = srv.URL
	return up
}

func mockQoderGateway(t *testing.T, replyText string, returnTool bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			raw, _ := io.ReadAll(r.Body)
			decoded, err := cosy.Decode(string(raw))
			if err != nil {
				t.Fatalf("expected COSY-encoded body: %v", err)
			}
			var qReq map[string]any
			if json.Unmarshal(decoded, &qReq) != nil {
				t.Fatalf("expected JSON body inside COSY payload")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if returnTool {
				c1 := `{"id":"chatcmpl-tool","model":"smodel","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]}}]}`
				c2 := `{"id":"chatcmpl-tool","model":"smodel","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`
				e1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": c1})
				e2, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": c2})
				_, _ = fmt.Fprintf(w, "data:%s\n\ndata:%s\n\ndata:{\"body\":\"[DONE]\"}\n\n", e1, e2)
				return
			}
			c1, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-1", "model": "smodel",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": replyText}}},
			})
			c2, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-1", "model": "smodel",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			e1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(c1)})
			e2, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(c2)})
			_, _ = fmt.Fprintf(w, "data:%s\n\ndata:%s\n\ndata:{\"body\":\"[DONE]\"}\n\n", e1, e2)
		case "/api/v1/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"u-pat-1","name":"PATUser","user_type":"personal_standard"}`)
		case "/api/v1/jobToken/exchange":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"jt-exchanged","refresh_token":"jrt-exchanged","expires_in":3600000}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestPortalAndMultiProtocolBridges(t *testing.T) {
	gw := mockQoderGateway(t, "Hello from Qoder!", false)
	defer gw.Close()

	tmp := t.TempDir()
	p := testPoolWithIDs("u1")
	up := upstream.New()
	up.HTTP = gw.Client()
	up.APIBaseGlobal = gw.URL
	up.APIBaseCN = gw.URL
	up.OpenAPIGlobal = gw.URL
	up.OpenAPICN = gw.URL

	um, _ := usermgr.New(filepath.Join(tmp, "users.json"), "admin123", "sk-master-key")
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		UserMgr:       um,
		APIKey:        "sk-master-key",
		GlobalEnabled: true,
		AuthDir:       tmp,
		StateFile:     filepath.Join(tmp, "state.json"),
		Version:       "v1.0.0-test",
	})

	// 1. Public Portal HTML + Public Status JSON (no auth)
	recPortal := httptest.NewRecorder()
	h.ServeHTTP(recPortal, httptest.NewRequest("GET", "/", nil))
	if recPortal.Code != 200 || !strings.Contains(recPortal.Body.String(), "Qoder") {
		t.Fatalf("unexpected portal page response: code=%d", recPortal.Code)
	}
	recPub := httptest.NewRecorder()
	h.ServeHTTP(recPub, httptest.NewRequest("GET", "/ui/public/status", nil))
	if recPub.Code != 200 || !strings.Contains(recPub.Body.String(), `"status":"online"`) {
		t.Fatalf("unexpected public status: code=%d body=%s", recPub.Code, recPub.Body.String())
	}

	// 2. OpenAI /v1/models & /v1/chat/completions
	recMod := httptest.NewRecorder()
	reqMod := httptest.NewRequest("GET", "/v1/models", nil)
	reqMod.Header.Set("Authorization", "Bearer sk-master-key")
	h.ServeHTTP(recMod, reqMod)
	if recMod.Code != 200 || !strings.Contains(recMod.Body.String(), `"auto-free"`) {
		t.Fatalf("unexpected /v1/models: code=%d body=%s", recMod.Code, recMod.Body.String())
	}

	reqChat := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"Sonus","messages":[{"role":"user","content":"hi"}]}`)))
	reqChat.Header.Set("Authorization", "Bearer sk-master-key")
	recChat := httptest.NewRecorder()
	h.ServeHTTP(recChat, reqChat)
	if recChat.Code != 200 || !strings.Contains(recChat.Body.String(), "Hello from Qoder!") {
		t.Fatalf("OpenAI sync chat failed: code=%d body=%s", recChat.Code, recChat.Body.String())
	}

	// 3. Anthropic /v1/messages (x-api-key) + /v1/messages/count_tokens
	reqClaude := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{"model":"smodel","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)))
	reqClaude.Header.Set("x-api-key", "sk-master-key")
	recClaude := httptest.NewRecorder()
	h.ServeHTTP(recClaude, reqClaude)
	if recClaude.Code != 200 || !strings.Contains(recClaude.Body.String(), `"type":"message"`) || !strings.Contains(recClaude.Body.String(), "Hello from Qoder!") {
		t.Fatalf("Anthropic /v1/messages failed: code=%d body=%s", recClaude.Code, recClaude.Body.String())
	}

	reqCount := httptest.NewRequest("POST", "/v1/messages/count_tokens", bytes.NewReader([]byte(`{"model":"smodel","messages":[{"role":"user","content":"count me"}]}`)))
	reqCount.Header.Set("x-api-key", "sk-master-key")
	recCount := httptest.NewRecorder()
	h.ServeHTTP(recCount, reqCount)
	if recCount.Code != 200 || !strings.Contains(recCount.Body.String(), `"input_tokens"`) {
		t.Fatalf("count_tokens failed: code=%d body=%s", recCount.Code, recCount.Body.String())
	}

	// 4. Codex /v1/responses (sync + namespace tool flattening)
	reqResp := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"global:smodel","instructions":"sys","input":"ping","tools":[{"type":"namespace","name":"fs","tools":[{"name":"read","description":"r","parameters":{"type":"object"}}]}]}`)))
	reqResp.Header.Set("Authorization", "Bearer sk-master-key")
	recResp := httptest.NewRecorder()
	h.ServeHTTP(recResp, reqResp)
	if recResp.Code != 200 || !strings.Contains(recResp.Body.String(), `"output_text"`) || !strings.Contains(recResp.Body.String(), "Hello from Qoder!") {
		t.Fatalf("Codex /v1/responses failed: code=%d body=%s", recResp.Code, recResp.Body.String())
	}

	// 5. Web OAuth Start & PAT import & ZIP Backup
	reqOAuth := httptest.NewRequest("POST", "/ui/oauth/start", bytes.NewReader([]byte(`{"realm":"cn"}`)))
	recOAuth := httptest.NewRecorder()
	h.ServeHTTP(recOAuth, reqOAuth)
	if recOAuth.Code != 200 || !strings.Contains(recOAuth.Body.String(), "qoder.com.cn") {
		t.Fatalf("OAuth start failed: code=%d body=%s", recOAuth.Code, recOAuth.Body.String())
	}

	reqPAT := httptest.NewRequest("POST", "/ui/oauth/import", bytes.NewReader([]byte(`{"raw":"pt-test-personal-token-xyz","realm":"global"}`)))
	recPAT := httptest.NewRecorder()
	h.ServeHTTP(recPAT, reqPAT)
	if recPAT.Code != 200 || !strings.Contains(recPAT.Body.String(), "u-pat-1") {
		t.Fatalf("PAT import failed: code=%d body=%s", recPAT.Code, recPAT.Body.String())
	}

	reqZip := httptest.NewRequest("GET", "/ui/action/backup", nil)
	recZip := httptest.NewRecorder()
	h.ServeHTTP(recZip, reqZip)
	if recZip.Code != 200 || !strings.Contains(recZip.Header().Get("Content-Type"), "zip") || recZip.Body.Len() == 0 {
		t.Fatalf("ZIP backup failed: code=%d ct=%s", recZip.Code, recZip.Header().Get("Content-Type"))
	}
}

func TestAnthropicStreamingToolUseBridge(t *testing.T) {
	gw := mockQoderGateway(t, "", true)
	defer gw.Close()

	p := testPoolWithIDs("u1")
	up := upstream.New()
	up.HTTP = gw.Client()
	up.APIBaseGlobal = gw.URL
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{"model":"smodel","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"read main.go"}]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"read_file"`) || !strings.Contains(body, `input_json_delta`) {
		t.Fatalf("expected Anthropic streaming tool_use events, got code=%d body=%s", rec.Code, body)
	}
}
