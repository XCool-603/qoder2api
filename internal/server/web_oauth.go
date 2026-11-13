package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/upstream"
)

type pendingOAuthSession struct {
	State       string
	Realm       string
	Platform    string
	VPCEndpoint string
	Nonce       string
	Verifier    string
	AuthURL     string
	CreatedAt   time.Time
	Owner       string
}

const oauthPendingTTL = 15 * time.Minute

func (h *Handler) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Realm       string `json:"realm"`
		Platform    string `json:"platform"`
		VPCEndpoint string `json:"vpc_endpoint"`
	}
	_ = json.Unmarshal(body, &req)

	realm := upstream.NormalizeRealm(req.Realm)
	if realm == upstream.RealmGlobal && !h.cfg.GlobalEnabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "global realm is disabled in config"})
		return
	}
	if req.Platform == "" {
		req.Platform = "CLI"
	}

	res, err := h.cfg.Upstream.StartOAuth(realm, req.Platform, req.VPCEndpoint, h.requestOwner(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	owner := "public"
	if u := h.getWebSessionUser(r); u != nil && u.Username != "" {
		owner = u.Username
	}
	sess := &pendingOAuthSession{
		State:       res.State,
		Realm:       res.Realm,
		Platform:    res.Platform,
		VPCEndpoint: res.VPCEndpoint,
		Nonce:       res.Nonce,
		Verifier:    res.Verifier,
		AuthURL:     res.AuthURL,
		CreatedAt:   time.Now(),
		Owner:       owner,
	}

	h.oauthMu.Lock()
	// Prune stale sessions
	for k, s := range h.oauthSessions {
		if time.Since(s.CreatedAt) > oauthPendingTTL {
			delete(h.oauthSessions, k)
		}
	}
	h.oauthSessions[res.State] = sess
	h.oauthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"url":          res.AuthURL,
		"state":        res.State,
		"authUrl":      res.AuthURL,
		"auth_url":     res.AuthURL,
		"realm":        res.Realm,
		"platform":     res.Platform,
		"vpc_endpoint": res.VPCEndpoint,
	})
}

func (h *Handler) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var state string
	if r.Method == http.MethodGet {
		state = strings.TrimSpace(r.URL.Query().Get("state"))
	} else {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		state = strings.TrimSpace(req.State)
	}
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "success": false, "status": "error", "error": "missing state"})
		return
	}

	h.oauthMu.Lock()
	sess, ok := h.oauthSessions[state]
	h.oauthMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "success": false, "status": "unknown", "error": "OAuth state not found or expired"})
		return
	}
	if time.Since(sess.CreatedAt) > oauthPendingTTL {
		h.oauthMu.Lock()
		delete(h.oauthSessions, state)
		h.oauthMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "success": false, "status": "expired", "error": "OAuth login window expired"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	acct, pending, err := h.cfg.Upstream.PollOAuthOnce(ctx, sess.Realm, sess.VPCEndpoint, sess.Nonce, sess.Verifier)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "success": false, "status": "error", "error": err.Error()})
		return
	}
	if pending || acct == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"success": false,
			"status":  "pending",
			"notice":  "等待用户在浏览器中确认授权...",
		})
		return
	}

	// Authorized -> persist to auths/, insert into pool, fetch initial quota breakdown asynchronously
	acct.Platform = sess.Platform
	acct.Owner = sess.Owner
	if acct.ProviderValue() == "" {
		acct.SetProvider("oauth")
	}
	h.saveNewAuthAccount(acct)

	h.oauthMu.Lock()
	delete(h.oauthSessions, state)
	h.oauthMu.Unlock()

	go h.WarmupSingleAccountCredits(acct)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"success":  true,
		"status":   "done",
		"uid":      acct.UID,
		"nickname": acct.Nickname,
		"realm":    acct.Realm(),
		"domain":   acct.Domain,
	})
}

func (h *Handler) handleOAuthCancel(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(body, &req)
	if req.State != "" {
		h.oauthMu.Lock()
		delete(h.oauthSessions, req.State)
		h.oauthMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) saveNewAuthAccount(acct *auth.Auth) {
	if h == nil || acct == nil {
		return
	}
	dir := h.getAuthDir()
	fp := resolveAuthFilePath(dir, acct.Realm(), acct.UID, acct.ProviderValue(), acct.AccessTokenValue(), acct.RefreshTokenValue())
	acct.FilePath = fp
	acct.BackfillRealm()
	_ = acct.SaveAtomic()
	if h.cfg.Pool != nil {
		h.cfg.Pool.Add(acct)
	}
}

// WarmupSingleAccountCredits syncs initial credit breakdown & expiring slice immediately after login/import.
func (h *Handler) WarmupSingleAccountCredits(acct *auth.Auth) {
	if h == nil || h.cfg.Upstream == nil || h.cfg.Pool == nil || acct == nil {
		return
	}
	remain, detail, err := h.cfg.Upstream.UserResourceDetailed(acct, 7*24*time.Hour)
	if err != nil {
		return
	}
	exp := 0.0
	if detail != nil {
		exp = detail.Expiring
	}
	h.cfg.Pool.ReenableIfCredits(acct.UID, int64(remain))
	h.cfg.Pool.SetCreditsDetailed(acct.UID, int64(remain), int64(exp))
}
