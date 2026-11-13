package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

func TestVPCEndpointParsing(t *testing.T) {
	inst, web, oapi, gw, err := ParseVPCEndpoint("acme")
	if err != nil {
		t.Fatalf("ParseVPCEndpoint short instance failed: %v", err)
	}
	if inst != "acme" || web != "https://acme.vpc.qoder.com.cn" || oapi != "https://acme-openapi.vpc.qoder.com.cn" || gw != "https://acme-gateway.vpc.qoder.com.cn" {
		t.Fatalf("unexpected VPC hosts: %s | %s | %s | %s", inst, web, oapi, gw)
	}
	inst2, _, _, gw2, err := ParseVPCEndpoint("https://demo-gateway.vpc.qoder.com.cn/")
	if err != nil || inst2 != "demo" || gw2 != "https://demo-gateway.vpc.qoder.com.cn" {
		t.Fatalf("unexpected gateway VPC parse: inst=%s gw=%s err=%v", inst2, gw2, err)
	}
	if _, _, _, _, err := ParseVPCEndpoint("https://evil.example.com"); err == nil {
		t.Fatal("expected error for non-vpc.qoder.com.cn domain")
	}
}

func TestCatalogRoutingAndEffortNormalization(t *testing.T) {
	if r, _ := AutoRealmForModel("smodel"); r != RealmGlobal {
		t.Fatalf("expected smodel -> global, got %s", r)
	}
	if r, _ := AutoRealmForModel("Sonus"); r != RealmGlobal {
		t.Fatalf("expected Sonus -> global, got %s", r)
	}
	if r, _ := AutoRealmForModel("q37fmodel"); r != RealmCN {
		t.Fatalf("expected q37fmodel -> cn, got %s", r)
	}
	if r, _ := AutoRealmForModel("cn:auto"); r != RealmCN {
		t.Fatalf("expected cn:auto -> cn, got %s", r)
	}
	if !IsFreeModel("qfmodel", RealmCN) || IsFreeModel("smodel", RealmGlobal) {
		t.Fatal("unexpected free model classification")
	}

	meta, ok := CatalogMetaFor("dfmodel", RealmGlobal)
	if !ok {
		t.Fatal("expected dfmodel in embedded catalog")
	}
	eff, _ := NormalizeReasoningEffort("disabled", meta)
	if eff != "none" {
		t.Fatalf("expected disabled -> none, got %q", eff)
	}
}

func TestErrorClassifierNestedAndQoderCodes(t *testing.T) {
	queuedBody := `{"code":"403","message":"{\"code\":\"10605\",\"subCode\":\"RESOURCE_LIMIT\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":12,\\\"retryAfterSeconds\\\":7,\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}"}`
	ue := ClassifyError(403, queuedBody)
	if ue.Kind != ErrQueueWait || ue.QueueCount != 12 || ue.RetryAfter != 7*time.Second || ue.ModelKey != "qfmodel" {
		t.Fatalf("unexpected queued error classification: %+v", ue)
	}

	limitBody := `{"code":"403","message":"{\"code\":\"10605\",\"subCode\":\"RESOURCE_LIMIT\",\"message\":\"{\\\"isQueued\\\":false,\\\"serviceAvailable\\\":false,\\\"modelKey\\\":\\\"cmodel\\\",\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}"}`
	ue2 := ClassifyError(403, limitBody)
	if ue2.Kind != ErrModelBlocked {
		t.Fatalf("expected ErrModelBlocked, got %s", ue2.Kind)
	}

	if k := Classify(401, `{"code":"TOKEN_EXPIRE"}`); k != ErrSessionDead {
		t.Fatalf("expected ErrSessionDead, got %s", k)
	}
	if k := Classify(403, `{"code":110,"message":"Billing daily count exceeded"}`); k != ErrSoftRate {
		t.Fatalf("expected ErrSoftRate for code 110, got %s", k)
	}
	if k := Classify(400, `{"error":{"code":"DataInspectionFailed","message":"inappropriate content"}}`); k != ErrContentBlocked {
		t.Fatalf("expected ErrContentBlocked, got %s", k)
	}
}

func TestNormalizeQuotaPayloadBFS(t *testing.T) {
	raw := map[string]any{
		"userQuota": map[string]any{
			"remaining": float64(1800),
			"used":      float64(200),
			"total":     float64(2000),
		},
		"dedicatedResourcePackages": []any{
			map[string]any{
				"remaining": float64(500),
				"total":     float64(500),
				"expiresAt": float64(time.Now().Add(48 * time.Hour).Unix()),
				"displayLabels": []any{
					map[string]any{
						"dimension": "title",
						"valueI18n": map[string]any{"zh-CN": "SOTA 专属积分"},
					},
				},
			},
		},
		"orgResourcePackage": map[string]any{
			"remaining": float64(0),
			"cap":       float64(-1),
			"available": true,
		},
	}
	bd, ok := NormalizeQuotaPayload(raw, RealmGlobal)
	if !ok {
		t.Fatal("expected quota payload normalized")
	}
	if bd.Remain != 2300 || bd.Shared == nil || !bd.Shared.Unlimited {
		t.Fatalf("unexpected quota breakdown: %+v", bd)
	}
	exp := computeExpiringAmount(bd, 7*24*time.Hour)
	if exp != 500 {
		t.Fatalf("expected 500 expiring within 7d, got %v", exp)
	}
}

func TestStartOAuthURLRealms(t *testing.T) {
	c := New()
	cnRes, err := c.StartOAuth(RealmCN, "CLI", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	uCN, _ := url.Parse(cnRes.AuthURL)
	if !strings.Contains(uCN.Host, "qoder.com.cn") || uCN.Query().Get("redirect_uri") != RedirectURICN || !strings.Contains(uCN.Query().Get("nonce"), "-") {
		t.Fatalf("unexpected CN authorize URL: %s", cnRes.AuthURL)
	}

	glRes, err := c.StartOAuth(RealmGlobal, "CLI", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	uGL, _ := url.Parse(glRes.AuthURL)
	if uGL.Host != "qoder.com" || uGL.Query().Get("redirect_uri") != "" || strings.Contains(uGL.Query().Get("nonce"), "-") {
		t.Fatalf("unexpected Global authorize URL: %s", glRes.AuthURL)
	}
}

func TestRefreshAndCampaignCheckinMocked(t *testing.T) {
	var claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathDeviceRefresh:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"dt-new-token","refresh_token":"drt-new-ref","expires_in":2592000000}`)
		case PathCampaigns:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"showCampaign":true,"claimable":true,"campaigns":[{"campaignId":"daily_100","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`)
		case "/sash/api/v1/me/campaigns/daily_100/claim":
			atomic.AddInt32(&claimCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)
		case PathProEligibility:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"eligible":false}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.OpenAPIGlobal = srv.URL

	a := &auth.Auth{
		UID:          "u-test",
		AccessToken:  "dt-old",
		RefreshToken: "drt-old",
		Domain:       "qoder.com",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	a.SetRealm(RealmGlobal)
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}
	if a.AccessTokenValue() != "dt-new-token" || a.RefreshTokenValue() != "drt-new-ref" {
		t.Fatalf("refresh did not update tokens: %+v", a)
	}

	sum, err := c.DailyCheckinDetailed(context.Background(), a)
	if err != nil || !sum.OK || sum.Earned != 100 || atomic.LoadInt32(&claimCalls) != 1 {
		t.Fatalf("unexpected checkin summary: %+v err=%v", sum, err)
	}
}

func TestChatStreamContextAndEnvelopeUnwrapping(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded, err := cosy.Decode(string(raw))
		if err != nil {
			t.Fatalf("failed to decode COSY outbound body: %v", err)
		}
		var reqObj map[string]any
		if json.Unmarshal(decoded, &reqObj) != nil {
			t.Fatalf("decoded outbound body is not valid JSON")
		}
		biz, _ := reqObj["business"].(map[string]any)
		if biz["model_key"] != "kmodel_latest" {
			t.Fatalf("expected business.model_key=kmodel_latest, got %v", biz["model_key"])
		}
		if !strings.HasPrefix(r.Header.Get("authorization"), "Bearer COSY.") {
			t.Fatalf("missing COSY bearer signature: %s", r.Header.Get("authorization"))
		}

		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if n == 1 {
			// Simulate Qoder wrapped 418 provider error inside HTTP 200 SSE frame -> auto retry!
			_, _ = fmt.Fprintf(w, "data:{\"body\":\"provider_error transient\",\"statusCodeValue\":418}\n\n")
			return
		}
		chunk1 := `{"id":"chatcmpl-1","model":"kmodel_latest","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "}}]}`
		chunk2 := `{"id":"chatcmpl-1","model":"kmodel_latest","choices":[{"index":0,"delta":{"content":"Qoder!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
		env1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk1})
		env2, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk2})
		_, _ = fmt.Fprintf(w, "data:%s\n\n", env1)
		_, _ = fmt.Fprintf(w, "data:%s\n\n", env2)
		_, _ = fmt.Fprintf(w, "data:{\"body\":\"[DONE]\"}\n\n")
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.APIBaseGlobal = srv.URL

	a := &auth.Auth{UID: "u-stream", AccessToken: "dt-valid-123", Domain: "qoder.com", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	a.SetRealm(RealmGlobal)

	rc, status, _, err := c.ChatStreamContext(context.Background(), a, map[string]any{
		"model":    "Kimi-K3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "kmodel_latest", ChatMeta{ConversationID: "conv-1"}, "", nil)
	if err != nil || status != 200 {
		t.Fatalf("ChatStreamContext failed: status=%d err=%v", status, err)
	}
	defer rc.Close()

	agg, err := Aggregate(rc)
	if err != nil {
		t.Fatalf("Aggregate failed: %v", err)
	}
	choices := agg["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello Qoder!" {
		t.Fatalf("unexpected aggregated content: %v", msg["content"])
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected transient retry from wrapped 418 to 2 attempts, got %d", attempts)
	}
}
