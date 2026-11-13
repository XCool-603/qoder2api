package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
)

func TestHandleDashboardDataCreditSummary(t *testing.T) {
	tmpDir := t.TempDir()

	cnFile := filepath.Join(tmpDir, "qoder-cn-sum.json")
	cnAuthJSON := `{
		"uid": "cn_sum_1",
		"access_token": "mock_at_cn",
		"nickname": "CN-Account",
		"realm": "cn"
	}`
	if err := os.WriteFile(cnFile, []byte(cnAuthJSON), 0600); err != nil {
		t.Fatal(err)
	}

	glFile := filepath.Join(tmpDir, "qoder-gl-sum.json")
	glAuthJSON := `{
		"uid": "gl_sum_1",
		"access_token": "mock_at_gl",
		"nickname": "GL-Account",
		"realm": "global"
	}`
	if err := os.WriteFile(glFile, []byte(glAuthJSON), 0600); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	cnAuth, _ := auth.Parse([]byte(cnAuthJSON))
	cnAuth.FilePath = cnFile
	p.Add(cnAuth)

	glAuth, _ := auth.Parse([]byte(glAuthJSON))
	glAuth.FilePath = glFile
	p.Add(glAuth)

	p.SetCreditsDetailed("cn_sum_1", 200, 50)
	p.SetCreditsDetailed("gl_sum_1", 300, 0)
	p.CooldownFreeQuotaModel("gl_sum_1", time.Now().Add(time.Hour), "qfmodel", "free quota exhausted")
	p.NoteModelCost("cn_sum_1", "smodel", 10.0, 1000)
	p.SetCheckinResult("cn_sum_1", "ok", "")
	p.SetCheckinRunSummary("auto", 2, 1, 0, 0, 1)

	up := upstream.New()

	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		AuthDir:       tmpDir,
		GlobalEnabled: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	w := httptest.NewRecorder()
	h.handleDashboardData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Service        string                `json:"service"`
		CurrentVersion string                `json:"currentVersion"`
		Status         map[string]any        `json:"status"`
		CreditSummary  CreditSummaryPayload  `json:"creditSummary"`
		CheckinSummary CheckinSummaryPayload `json:"checkinSummary"`
		Accounts       []struct {
			UID           string           `json:"uid"`
			Realm         string           `json:"realm"`
			CheckinStatus string           `json:"checkin_status"`
			CreditDetail  *CreditBreakdown `json:"credit_detail"`
			InviteCode    string           `json:"invite_code"`
			InviteURL     string           `json:"invite_url"`
		} `json:"accounts"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}

	if resp.Service != ServiceName {
		t.Errorf("service = %q, want %q", resp.Service, ServiceName)
	}
	if resp.CurrentVersion != CurrentVersion {
		t.Errorf("currentVersion = %q, want %q", resp.CurrentVersion, CurrentVersion)
	}
	if len(resp.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2", len(resp.Accounts))
	}
	for _, a := range resp.Accounts {
		if a.CreditDetail == nil || len(a.CreditDetail.Packs) == 0 {
			t.Fatalf("account %s missing credit_detail packs: %+v", a.UID, a.CreditDetail)
		}
		if a.InviteCode == "" {
			t.Errorf("account %s missing invite_code", a.UID)
		}
		if a.InviteURL == "" {
			t.Errorf("account %s missing invite_url", a.UID)
		}
	}

	all := resp.CreditSummary.All
	if all.Remain != 490 {
		t.Errorf("all.Remain = %f, want 490", all.Remain)
	}
	if all.AccountCount != 2 {
		t.Errorf("all.AccountCount = %d, want 2", all.AccountCount)
	}
	if all.RateLimitedModelsCount != 1 {
		t.Errorf("all.RateLimitedModelsCount = %d, want 1", all.RateLimitedModelsCount)
	}

	cn := resp.CreditSummary.CN
	if cn.Remain != 190 {
		t.Errorf("cn.Remain = %f, want 190", cn.Remain)
	}

	gl := resp.CreditSummary.Global
	if gl.Remain != 300 {
		t.Errorf("gl.Remain = %f, want 300", gl.Remain)
	}

	if resp.CheckinSummary.TodayChecked != 1 || resp.CheckinSummary.CNAccounts != 1 || resp.CheckinSummary.OK != 1 {
		t.Errorf("checkinSummary mismatch: %+v", resp.CheckinSummary)
	}
}
