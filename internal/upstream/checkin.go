package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

// CampaignBenefit describes the credit/code reward of a Qoder campaign.
type CampaignBenefit struct {
	Kind   string `json:"kind"`
	Amount int    `json:"amount"`
}

// CampaignItem represents one entry from /sash/api/v1/me/campaigns.
type CampaignItem struct {
	CampaignID        string          `json:"campaign_id"`
	CampaignKey       string          `json:"campaign_key"`
	Title             string          `json:"title"`
	ActionType        string          `json:"action_type"`
	ClaimStatus       string          `json:"claim_status"` // CLAIMABLE / CLAIMED / NOT_ELIGIBLE / BLOCKED
	UnavailableReason string          `json:"unavailable_reason,omitempty"`
	EndAt             string          `json:"end_at,omitempty"`
	Benefit           CampaignBenefit `json:"benefit"`
}

// CampaignsSnapshot is the structured output from /sash/api/v1/me/campaigns.
type CampaignsSnapshot struct {
	OK           bool           `json:"ok"`
	Available    bool           `json:"available"`
	ShowCampaign bool           `json:"show_campaign"`
	Claimable    bool           `json:"claimable"`
	CampaignURL  string         `json:"campaign_url,omitempty"`
	Campaigns    []CampaignItem `json:"campaigns"`
	Error        string         `json:"error,omitempty"`
}

// ClaimResult represents the outcome of claiming a single campaign.
type ClaimResult struct {
	OK             bool   `json:"ok"`
	CampaignID     string `json:"campaign_id"`
	Status         string `json:"status"`
	Replayed       bool   `json:"replayed"`
	Blocked        bool   `json:"blocked,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	RedemptionCode string `json:"redemption_code,omitempty"`
	Amount         int    `json:"amount"`
	Message        string `json:"message"`
}

// CheckinSummary reports full campaign check-in results across all eligible rewards.
type CheckinSummary struct {
	OK      bool          `json:"ok"`
	Already bool          `json:"already"`
	Earned  int           `json:"earned"`
	Claimed []ClaimResult `json:"claimed,omitempty"`
	Message string        `json:"message"`
}

// applyDesktopCampaignHeaders sets Qoder 0.4.8 desktop headers for /sash/ endpoints.
// Crucial rule from Qoder reverse engineering: do NOT send derived fake 6 machine headers
// unless the account has a real captured desktop CheckinFingerprint (otherwise CLAIMABLE rows are hidden).
func (c *Client) applyDesktopCampaignHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Qoder")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("cosy-clienttype", "10")

	a.Lock()
	fp := a.CheckinFingerprint
	a.Unlock()

	if fp != nil && fp.MachineToken != "" {
		ver := fp.Version
		if ver == "" {
			ver = "0.4.8"
		}
		req.Header.Set("cosy-version", ver)
		req.Header.Set("cosy-machineos", cosy.MachineOS())
		if fp.Hostname != "" {
			req.Header.Set("cosy-machinehostname", fp.Hostname)
		}
		if fp.MachineID != "" {
			req.Header.Set("cosy-machineid", fp.MachineID)
		}
		req.Header.Set("cosy-machinetoken", fp.MachineToken)
		if fp.MachineType != "" {
			req.Header.Set("cosy-machinetype", fp.MachineType)
		}
		if fp.MachineCode != "" {
			req.Header.Set("cosy-machinecode", fp.MachineCode)
		}
		return
	}
	req.Header.Set("cosy-version", "0.4.8")
}

// ListCampaigns retrieves official Qoder campaign & daily check-in status.
func (c *Client) ListCampaigns(ctx context.Context, a *auth.Auth) (*CampaignsSnapshot, error) {
	if a == nil || a.AccessTokenValue() == "" {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "missing token"}
	}
	cfg := c.RealmConfigForAccount(a)
	u := strings.TrimRight(cfg.OpenAPI, "/") + PathCampaigns

	doFetch := func(desktop bool) (map[string]any, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		if desktop {
			c.applyDesktopCampaignHeaders(req, a)
		} else {
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", cfg.UserAgent)
			req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, 0, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, resp.StatusCode, ClassifyError(resp.StatusCode, string(raw))
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return nil, resp.StatusCode, fmt.Errorf("invalid campaigns JSON")
		}
		return m, resp.StatusCode, nil
	}

	m, status, err := doFetch(true)
	if err != nil && (status == 401 || status == 403) {
		if m2, st2, err2 := doFetch(false); err2 == nil {
			m, status, err = m2, st2, nil
		} else if st2 == 401 {
			return nil, err2
		}
	}
	if err != nil {
		return &CampaignsSnapshot{OK: false, Available: status != 404 && status != 405 && status != 410, Error: err.Error()}, err
	}

	snap := &CampaignsSnapshot{
		OK:          true,
		Available:   true,
		CampaignURL: anyStr(m["campaignUrl"]),
	}
	snap.ShowCampaign, _ = m["showCampaign"].(bool)
	snap.Claimable, _ = m["claimable"].(bool)

	if arr, ok := m["campaigns"].([]any); ok {
		for _, item := range arr {
			cm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var bene CampaignBenefit
			if bm, ok := cm["benefit"].(map[string]any); ok {
				bene.Kind = strings.ToUpper(anyStr(bm["kind"]))
				bene.Amount = anyInt(bm["amount"])
			}
			ci := CampaignItem{
				CampaignID:        firstNonEmptyStr(cm, "campaignId", "campaign_id", "id"),
				CampaignKey:       firstNonEmptyStr(cm, "campaignKey", "campaign_key", "key"),
				Title:             firstNonEmptyStr(cm, "title", "name"),
				ActionType:        firstNonEmptyStr(cm, "actionType", "action_type"),
				ClaimStatus:       strings.ToUpper(firstNonEmptyStr(cm, "claimStatus", "claim_status")),
				UnavailableReason: strings.ToUpper(firstNonEmptyStr(cm, "unavailableReason", "unavailable_reason")),
				EndAt:             firstNonEmptyStr(cm, "endAt", "end_at"),
				Benefit:           bene,
			}
			if ci.ClaimStatus == "CLAIMABLE" {
				snap.Claimable = true
			}
			snap.Campaigns = append(snap.Campaigns, ci)
		}
	}
	return snap, nil
}

// ClaimCampaign claims a single Qoder campaign reward by ID.
func (c *Client) ClaimCampaign(ctx context.Context, a *auth.Auth, campaignID string) (*ClaimResult, error) {
	if a == nil || a.AccessTokenValue() == "" {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "missing token"}
	}
	cfg := c.RealmConfigForAccount(a)
	u := strings.TrimRight(cfg.OpenAPI, "/") + fmt.Sprintf(PathCampaignClaim, campaignID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	c.applyDesktopCampaignHeaders(req, a)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return &ClaimResult{OK: true, CampaignID: campaignID, Status: "CLAIMED", Replayed: true, Message: "今日已领取（上游幂等确认）"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		ue := ClassifyError(resp.StatusCode, string(raw))
		return &ClaimResult{OK: false, CampaignID: campaignID, Message: ue.Msg}, ue
	}

	var r map[string]any
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("invalid claim JSON")
	}
	status := strings.ToUpper(anyStr(r["status"]))
	failure := strings.ToUpper(anyStr(r["failureCode"]))
	replayed, _ := r["replayed"].(bool)
	code := strings.TrimSpace(anyStr(r["redemptionCode"]))
	var amount int
	if bm, ok := r["benefit"].(map[string]any); ok {
		amount = anyInt(bm["amount"])
	}
	if amount == 0 {
		amount = anyInt(r["amount"])
	}

	if failure == "SAME_PERSON_ALREADY_CLAIMED" || status == "BLOCKED" {
		return &ClaimResult{
			OK:          false,
			CampaignID:  campaignID,
			Status:      "BLOCKED",
			Blocked:     true,
			FailureCode: failure,
			Amount:      amount,
			Message:     "同人已领取（同设备/身份下其他账号本轮已领）",
		}, nil
	}
	if code != "" {
		a.Lock()
		if a.CampaignCodes == nil {
			a.CampaignCodes = make(map[string]string)
		}
		a.CampaignCodes[campaignID] = code
		a.Unlock()
		_ = a.SaveAtomic()
	}
	ok := status == "CLAIMED" || status == "GRANTED" || status == "SUCCESS"
	msg := "领取成功"
	if replayed {
		msg = "今日已领取"
	}
	if code != "" {
		msg += "，兑换码：" + code
	}
	return &ClaimResult{
		OK:             ok,
		CampaignID:     campaignID,
		Status:         status,
		Replayed:       replayed,
		FailureCode:    failure,
		RedemptionCode: code,
		Amount:         amount,
		Message:        msg,
	}, nil
}

// CheckAndClaimProUpgrade probes and claims Qoder Pro trial/upgrade eligibility when offered.
func (c *Client) CheckAndClaimProUpgrade(ctx context.Context, a *auth.Auth) (bool, error) {
	if a == nil || a.AccessTokenValue() == "" {
		return false, nil
	}
	cfg := c.RealmConfigForAccount(a)
	base := strings.TrimRight(cfg.OpenAPI, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PathProEligibility, nil)
	if err != nil {
		return false, err
	}
	c.applyDesktopCampaignHeaders(req, a)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var elig map[string]any
	if json.Unmarshal(raw, &elig) != nil {
		return false, nil
	}
	canUpgrade, _ := elig["eligible"].(bool)
	if !canUpgrade {
		if v, ok := elig["canClaim"].(bool); ok {
			canUpgrade = v
		}
	}
	if !canUpgrade {
		return false, nil
	}

	creq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+PathProClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, err
	}
	c.applyDesktopCampaignHeaders(creq, a)
	creq.Header.Set("Content-Type", "application/json")
	cresp, err := c.httpClient().Do(creq)
	if err != nil {
		return false, err
	}
	craw, _ := io.ReadAll(io.LimitReader(cresp.Body, 512<<10))
	cresp.Body.Close()
	if cresp.StatusCode == http.StatusOK {
		c.emitActivity("pro_upgrade", true, a.UID, a.Nickname, NormalizeRealm(a.Realm()), "Pro upgrade claimed", 0)
		return true, nil
	}
	c.emitActivity("pro_upgrade", false, a.UID, a.Nickname, NormalizeRealm(a.Realm()), fmt.Sprintf("HTTP %d: %s", cresp.StatusCode, string(craw[:minInt(len(craw), 120)])), 0)
	return false, nil
}

// DailyCheckinDetailed performs full campaign list + retry + claim workflow for one account.
func (c *Client) DailyCheckinDetailed(ctx context.Context, a *auth.Auth) (*CheckinSummary, error) {
	if a == nil || a.AccessTokenValue() == "" {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "missing access token"}
	}
	snap, err := c.ListCampaigns(ctx, a)
	if err != nil {
		c.emitActivity("checkin", false, a.UID, a.Nickname, NormalizeRealm(a.Realm()), err.Error(), 0)
		return nil, err
	}
	hasActionable := func(s *CampaignsSnapshot) bool {
		if s == nil {
			return false
		}
		for _, item := range s.Campaigns {
			if item.ClaimStatus == "CLAIMABLE" || item.ClaimStatus == "CLAIMED" {
				return true
			}
		}
		return false
	}
	// Qoder upstream intermittently omits campaigns on first request (~20% of calls); retry twice briefly.
	if !hasActionable(snap) {
		for _, wait := range []time.Duration{800 * time.Millisecond, 2 * time.Second} {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			if retrySnap, rerr := c.ListCampaigns(ctx, a); rerr == nil && hasActionable(retrySnap) {
				snap = retrySnap
				break
			}
		}
	}

	sum := &CheckinSummary{OK: true}
	var alreadyCount int
	var firstErr error
	for _, camp := range snap.Campaigns {
		switch camp.ClaimStatus {
		case "CLAIMED":
			if camp.Benefit.Amount > 0 || camp.ActionType == "" || camp.ActionType == "CLAIM_BENEFIT" {
				alreadyCount++
			}
		case "CLAIMABLE":
			res, cerr := c.ClaimCampaign(ctx, a, camp.CampaignID)
			if res != nil {
				sum.Claimed = append(sum.Claimed, *res)
				if res.OK && !res.Replayed {
					sum.Earned += res.Amount
				}
				if res.Replayed {
					alreadyCount++
				}
			}
			if cerr != nil && firstErr == nil {
				firstErr = cerr
			}
			time.Sleep(1 * time.Second)
		}
	}

	_, _ = c.CheckAndClaimProUpgrade(ctx, a)

	if sum.Earned > 0 {
		sum.Message = fmt.Sprintf("签到领取成功 +%d Credits", sum.Earned)
		stampAccountCheckin(a)
		c.emitActivity("checkin", true, a.UID, a.Nickname, NormalizeRealm(a.Realm()), sum.Message, sum.Earned)
		return sum, nil
	}
	if alreadyCount > 0 {
		sum.Already = true
		sum.Message = "今日已签到领取"
		stampAccountCheckin(a)
		c.emitActivity("checkin", true, a.UID, a.Nickname, NormalizeRealm(a.Realm()), sum.Message, 0)
		return sum, nil
	}
	if firstErr != nil {
		sum.OK = false
		sum.Message = firstErr.Error()
		c.emitActivity("checkin", false, a.UID, a.Nickname, NormalizeRealm(a.Realm()), sum.Message, 0)
		return sum, firstErr
	}
	sum.Message = "当前暂无可领取的官方活动"
	return sum, nil
}

// DailyCheckin matches the scheduler's simple error contract: nil = freshly claimed,
// IsAlreadyCheckin(err) = already claimed today, else genuine error.
func (c *Client) DailyCheckin(a *auth.Auth) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sum, err := c.DailyCheckinDetailed(ctx, a)
	if err != nil {
		return err
	}
	if sum != nil && sum.Already {
		return fmt.Errorf("already checked in today")
	}
	return nil
}

func stampAccountCheckin(a *auth.Auth) {
	if a == nil {
		return
	}
	a.Lock()
	a.LastCheckinAt = time.Now().Unix()
	a.Unlock()
	_ = a.SaveAtomic()
}
