package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/upstream"
)

// handleOAuthImport handles single credential or PAT (pt-...) import from web console.
func (h *Handler) handleOAuthImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		Raw         string `json:"raw"`
		Token       string `json:"token"`
		Content     string `json:"content"`
		Realm       string `json:"realm"`
		Nickname    string `json:"nickname"`
		VPCEndpoint string `json:"vpc_endpoint"`
	}
	_ = json.Unmarshal(body, &req)
	rawInput := strings.TrimSpace(req.Raw)
	if rawInput == "" {
		rawInput = strings.TrimSpace(req.Token)
	}
	if rawInput == "" {
		rawInput = strings.TrimSpace(req.Content)
	}
	if rawInput == "" {
		rawInput = strings.TrimSpace(string(body))
	}

	acct, err := h.importSingleCredential(r.Context(), rawInput, req.Realm, req.Nickname, req.VPCEndpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      acct.UID,
		"nickname": acct.Nickname,
		"realm":    acct.Realm(),
		"domain":   acct.Domain,
	})
}

func (h *Handler) importSingleCredential(ctx context.Context, rawInput, explicitRealm, nickname, vpcEndpoint string) (*auth.Auth, error) {
	trimmed := strings.TrimSpace(rawInput)
	low := strings.ToLower(trimmed)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil, fmt.Errorf("检测到输入的是网页授权登录链接（URL），不是有效的 Token！请在浏览器中打开该链接完成登录授权；如需手工导入凭证，请输入真正的 access_token、refresh_token 或 pt- 开头的个人访问令牌")
	}

	a, err := auth.Parse([]byte(rawInput))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.ToLower(a.AccessTokenValue()), "http://") || strings.HasPrefix(strings.ToLower(a.AccessTokenValue()), "https://") {
		return nil, fmt.Errorf("检测到凭证中 AccessToken 误存为网页授权链接（URL），不是有效 Token！")
	}
	if explicitRealm != "" {
		a.SetRealm(upstream.NormalizeRealm(explicitRealm))
		if a.Realm() == "cn" {
			a.Domain = "qoder.com.cn"
		} else {
			a.Domain = "qoder.com"
		}
	}
	if vpcEndpoint != "" {
		if _, _, _, _, err := upstream.ParseVPCEndpoint(vpcEndpoint); err != nil {
			return nil, err
		}
		a.VPCEndpoint = vpcEndpoint
	}
	if nickname != "" {
		a.Nickname = nickname
	}

	// PAT (pt-...) exchange -> jt-/jrt- session + userinfo
	atVal := a.AccessTokenValue()
	if a.PersonalTokenValue() != "" && (atVal == "" || atVal == "pending_refresh") && h.cfg.Upstream != nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := h.cfg.Upstream.ExchangePersonalToken(cctx, a); err != nil {
			return nil, fmt.Errorf("PAT exchange failed: %w", err)
		}
	} else if atVal != "" && atVal != "pending_refresh" && (a.UID == "" || strings.HasPrefix(a.UID, "token_") || strings.HasPrefix(a.UID, "qd-") || a.Nickname == "") && h.cfg.Upstream != nil {
		// Enrich UID & Nickname from /api/v1/userinfo when importing raw token lines
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if ui, uerr := h.cfg.Upstream.FetchUserInfo(cctx, a); uerr == nil && ui != nil {
			if (a.UID == "" || strings.HasPrefix(a.UID, "token_") || strings.HasPrefix(a.UID, "qd-")) && ui.ID != "" {
				a.UID = ui.ID
			}
			if a.Nickname == "" {
				if ui.Nickname != "" {
					a.Nickname = ui.Nickname
				} else if ui.Name != "" {
					a.Nickname = ui.Name
				} else if ui.Email != "" {
					a.Nickname = ui.Email
				}
			}
			if ui.UserType != "" {
				a.UserType = ui.UserType
			}
			a.OrganizationID = ui.OrganizationID
			a.OrganizationName = ui.OrganizationName
		}
	}

	if a.UID == "" {
		a.UID = "qd-" + cosy.NewSimpleID()[:12]
	}
	if a.Owner == "" {
		a.Owner = "public"
	}
	h.saveNewAuthAccount(a)
	go h.WarmupSingleAccountCredits(a)
	return a, nil
}

// handleOAuthBatchImport imports JSON arrays, multi-line TXT/PAT files, or ZIP backups.
func (h *Handler) handleOAuthBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	var payloadBlobs [][]byte
	var explicitRealm string
	var dryRun bool

	if strings.Contains(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		explicitRealm = r.FormValue("realm")
		dryRun = r.FormValue("dry_run") == "true" || r.FormValue("dryRun") == "true"
		if txt := strings.TrimSpace(r.FormValue("content")); txt != "" {
			payloadBlobs = append(payloadBlobs, []byte(txt))
		}
		for _, hdrs := range r.MultipartForm.File {
			for _, fh := range hdrs {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, _ := io.ReadAll(io.LimitReader(f, 16<<20))
				_ = f.Close()
				if strings.HasSuffix(strings.ToLower(fh.Filename), ".zip") {
					zr, zerr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
					if zerr == nil {
						for _, zf := range zr.File {
							if zf.FileInfo().IsDir() || strings.Contains(zf.Name, "..") {
								continue
							}
							rc, err := zf.Open()
							if err != nil {
								continue
							}
							zb, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
							_ = rc.Close()
							if len(zb) > 0 {
								payloadBlobs = append(payloadBlobs, zb)
							}
						}
					}
				} else if len(data) > 0 {
					payloadBlobs = append(payloadBlobs, data)
				}
			}
		}
	} else {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		var req struct {
			Content string `json:"content"`
			Raw     string `json:"raw"`
			Realm   string `json:"realm"`
			DryRun  bool   `json:"dry_run"`
			Items   []any  `json:"items"`
		}
		if json.Unmarshal(raw, &req) == nil && (req.Content != "" || req.Raw != "" || len(req.Items) > 0) {
			explicitRealm = req.Realm
			dryRun = req.DryRun
			if req.Content != "" {
				payloadBlobs = append(payloadBlobs, []byte(req.Content))
			}
			if req.Raw != "" {
				payloadBlobs = append(payloadBlobs, []byte(req.Raw))
			}
			for _, it := range req.Items {
				if b, err := json.Marshal(it); err == nil {
					payloadBlobs = append(payloadBlobs, b)
				}
			}
		} else {
			payloadBlobs = append(payloadBlobs, raw)
		}
	}

	var imported, skipped, failed int
	var errorsList []string
	var previews []map[string]any

	processItem := func(itemBytes []byte) {
		trimmed := bytes.TrimSpace(itemBytes)
		if len(trimmed) == 0 {
			return
		}
		if dryRun {
			a, err := auth.Parse(trimmed)
			if err != nil {
				failed++
				errorsList = append(errorsList, err.Error())
				return
			}
			if explicitRealm != "" {
				a.SetRealm(upstream.NormalizeRealm(explicitRealm))
			}
			imported++
			previews = append(previews, map[string]any{
				"uid":      a.UID,
				"nickname": a.Nickname,
				"realm":    a.Realm(),
				"has_pat":  a.PersonalTokenValue() != "",
			})
			return
		}
		acct, err := h.importSingleCredential(r.Context(), string(trimmed), explicitRealm, "", "")
		if err != nil {
			failed++
			errorsList = append(errorsList, err.Error())
			return
		}
		if acct != nil {
			imported++
		} else {
			skipped++
		}
	}

	for _, blob := range payloadBlobs {
		clean := auth.CleanRawInput(blob)
		if len(clean) == 0 {
			continue
		}
		// JSON Array
		if clean[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(clean, &arr) == nil {
				for _, elem := range arr {
					processItem(elem)
				}
				continue
			}
		}
		// Single JSON object or wrapper {"accounts": [...]}
		if clean[0] == '{' {
			var wrap struct {
				Accounts []json.RawMessage `json:"accounts"`
			}
			if json.Unmarshal(clean, &wrap) == nil && len(wrap.Accounts) > 0 {
				for _, elem := range wrap.Accounts {
					processItem(elem)
				}
				continue
			}
			processItem(clean)
			continue
		}
		// Line-by-line JSON / PAT / card format
		for _, line := range strings.Split(string(clean), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			processItem([]byte(line))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"dry_run":  dryRun,
		"imported": imported,
		"skipped":  skipped,
		"failed":   failed,
		"errors":   errorsList,
		"previews": previews,
	})
}

// handleAccountUpdateToken replaces an account's access/refresh/PAT tokens cleanly.
func (h *Handler) handleAccountUpdateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		UID           string `json:"uid"`
		AccessToken   string `json:"access_token"`
		RefreshToken  string `json:"refresh_token"`
		PersonalToken string `json:"personal_token"`
		Realm         string `json:"realm"`
	}
	if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.UID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing uid or JSON"})
		return
	}
	a := h.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	a.Lock()
	if strings.TrimSpace(req.AccessToken) != "" {
		a.AccessToken = strings.TrimSpace(req.AccessToken)
		if strings.HasPrefix(a.AccessToken, "pt-") && a.PersonalToken == "" {
			a.PersonalToken = a.AccessToken
			a.AccessToken = ""
		}
	}
	if strings.TrimSpace(req.RefreshToken) != "" {
		a.RefreshToken = strings.TrimSpace(req.RefreshToken)
	}
	if strings.TrimSpace(req.PersonalToken) != "" {
		a.PersonalToken = strings.TrimSpace(req.PersonalToken)
	}
	if req.Realm != "" {
		a.Unlock()
		a.SetRealm(upstream.NormalizeRealm(req.Realm))
	} else {
		a.Unlock()
	}
	if a.PersonalTokenValue() != "" && a.AccessTokenValue() == "" && h.cfg.Upstream != nil {
		if err := h.cfg.Upstream.ExchangePersonalToken(r.Context(), a); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	_ = a.SaveAtomic()
	if h.cfg.Upstream != nil && h.cfg.Upstream.Sessions != nil {
		h.cfg.Upstream.Sessions.Invalidate(a.UID)
	}
	h.cfg.Pool.ClearSessionDead(a.UID)
	h.cfg.Pool.ReviveDisabled(a.UID)
	h.cfg.Pool.SetManualDisabled(a.UID, false, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a.UID})
}

// handleActionBackup exports all credential JSONs cleanly in a downloadable ZIP archive.
func (h *Handler) handleActionBackup(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if h.cfg.Pool != nil {
		for _, uid := range h.cfg.Pool.AvailableUIDs() {
			a := h.cfg.Pool.AuthByUID(uid)
			if a == nil {
				continue
			}
			realmVal := a.RealmStored()
			a.Lock()
			payload := map[string]any{
				"account": map[string]any{
					"uid":              a.UID,
					"nickname":         a.Nickname,
					"userType":         a.UserType,
					"organizationId":   a.OrganizationID,
					"organizationName": a.OrganizationName,
				},
				"auth": map[string]any{
					"accessToken":   a.AccessToken,
					"refreshToken":  a.RefreshToken,
					"personalToken": a.PersonalToken,
					"expiresAt":     a.ExpiresAt,
					"domain":        a.Domain,
					"realm":         realmVal,
					"vpcEndpoint":   a.VPCEndpoint,
					"platform":      a.Platform,
				},
			}
			a.Unlock()
			raw, _ := json.MarshalIndent(payload, "", "  ")
			fname := fmt.Sprintf("qoder-%s-%s.json", a.Realm(), auth.SanitizeFilename(a.UID))
			if a.FilePath != "" {
				fname = filepath.Base(a.FilePath)
			}
			if f, err := zw.Create(fname); err == nil {
				_, _ = f.Write(raw)
			}
		}
	}
	_ = zw.Close()

	filename := fmt.Sprintf("qoder2api-accounts-%s.zip", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
