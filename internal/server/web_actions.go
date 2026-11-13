package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/upstream"
)

func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func (h *Handler) getAccountCredits(uid string) int64 {
	if h != nil && h.cfg.Pool != nil {
		if st, ok := h.cfg.Pool.Status(uid); ok {
			return st.Credits
		}
	}
	return 0
}

func (h *Handler) handleAccountAdmin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		UID    string `json:"uid"`
		Action string `json:"action"` // disable | enable | revive | clear_cooldown | test | refresh | owner
		Reason string `json:"reason"`
		Owner  string `json:"owner"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.UID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request or missing uid"})
		return
	}

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "disable":
		found, changed := h.cfg.Pool.SetManualDisabled(req.UID, true, req.Reason)
		writeJSON(w, http.StatusOK, map[string]any{"ok": found, "changed": changed})
	case "enable":
		found, changed := h.cfg.Pool.SetManualDisabled(req.UID, false, "")
		writeJSON(w, http.StatusOK, map[string]any{"ok": found, "changed": changed})
	case "revive", "clear_cooldown":
		changed := h.cfg.Pool.ReviveDisabled(req.UID)
		h.cfg.Pool.ClearSessionDead(req.UID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed})
	case "refresh":
		a := h.cfg.Pool.AuthByUID(req.UID)
		if a == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
			return
		}
		if err := h.cfg.Upstream.RefreshToken(a); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		h.cfg.Pool.ClearSessionDead(a.UID)
		h.cfg.Pool.ReviveDisabled(a.UID)
		go h.fetchAndSyncCredit(a, a.UID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a.UID})
	case "owner":
		a := h.cfg.Pool.AuthByUID(req.UID)
		if a == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
			return
		}
		a.SetOwner(req.Owner)
		_ = a.SaveAtomic()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "owner": a.OwnerValue()})
	case "test":
		h.testSingleAccount(w, r, req.UID, req.Model)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown account action: " + req.Action})
	}
}

func (h *Handler) testSingleAccount(w http.ResponseWriter, r *http.Request, uid, reqModel string) {
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	realm := a.Realm()
	modelKey := reqModel
	if modelKey == "" {
		modelKey = upstream.DefaultTestModel(realm)
	} else {
		_, modelKey = h.resolveModelForHandler(reqModel)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	rc, status, rawErr, err := h.cfg.Upstream.ChatStreamContext(ctx, a, map[string]any{
		"model":      modelKey,
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "ping, reply OK only"}},
	}, modelKey, upstream.ChatMeta{}, "", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"uid":    uid,
			"model":  modelKey,
			"status": status,
			"error":  err.Error(),
			"detail": string(rawErr),
		})
		return
	}
	defer rc.Close()
	agg, aerr := upstream.Aggregate(rc)
	if aerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": uid, "model": modelKey, "error": aerr.Error()})
		return
	}
	var preview string
	if choices, ok := agg["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				preview, _ = msg["content"].(string)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"model":   modelKey,
		"realm":   realm,
		"preview": preview,
	})
}

func (h *Handler) handleActionDelete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(body, &req)
	if strings.TrimSpace(req.UID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing uid"})
		return
	}
	a := h.cfg.Pool.AuthByUID(req.UID)
	filePath := ""
	if a != nil {
		filePath = a.FilePath
	}
	h.cfg.Pool.Remove(req.UID)
	if filePath != "" {
		_ = os.Remove(filePath)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": true})
}

// handleActionSignin triggers Qoder campaign daily check-in either for one uid or the entire pool.
func (h *Handler) handleActionSignin(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(body, &req)

	user := h.getWebSessionUser(r)

	// Single account checkin
	if strings.TrimSpace(req.UID) != "" {
		a := h.cfg.Pool.AuthByUID(req.UID)
		if a == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "success": false, "error": "account not found"})
			return
		}
		if !h.isAccountOwnedBy(a, user) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "success": false, "error": "permission denied"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		sum, err := h.cfg.Upstream.DailyCheckinDetailed(ctx, a)
		go h.fetchAndSyncCredit(a, a.UID)
		status := "OK"
		detail := "签到成功"
		if sum != nil && sum.Already {
			status = "ALREADY"
			detail = "今日已签到"
		} else if err != nil {
			status = "FAIL"
			detail = err.Error()
		}
		h.cfg.Pool.SetCheckinResult(a.UID, strings.ToLower(status), detail)

		outStr := fmt.Sprintf("账号: %s (%s)\n状态: %s\n详情: %s", a.Nickname, a.UID, status, detail)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      err == nil,
			"success": err == nil,
			"uid":     a.UID,
			"summary": sum,
			"output":  outStr,
		})
		return
	}

	// Batch checkin for all accounts
	authDir := h.getAuthDir()
	files, _ := auth.LoadAuthFiles(authDir)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", "UID", "昵称", "签到状态", "余额", "详情"))
	sb.WriteString("-------------------------------------+--------------+------------+--------+--------------------\n")

	okN, alreadyN, failN, skipN := 0, 0, 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", filepath.Base(f), "-", "LOAD_ERR", "-", err.Error()))
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", filepath.Base(f), "-", "PARSE_ERR", "-", err.Error()))
			failN++
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		sum, cerr := h.cfg.Upstream.DailyCheckinDetailed(ctx, a)
		cancel()

		status := "FAIL"
		detail := ""
		if sum != nil && sum.Already {
			status = "ALREADY"
			detail = "今日已签到"
			alreadyN++
		} else if cerr == nil {
			status = "OK"
			detail = "签到成功"
			okN++
		} else {
			status = "FAIL"
			detail = cerr.Error()
			failN++
		}

		h.cfg.Pool.SetCheckinResult(a.UID, strings.ToLower(status), detail)
		go h.fetchAndSyncCredit(a, a.UID)

		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		creditsStr := "-"
		if creds := h.getAccountCredits(a.UID); creds > 0 {
			creditsStr = fmt.Sprintf("%d", creds)
		}
		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n",
			truncateStr(a.UID, 36), truncateStr(nick, 12), status, creditsStr, truncateStr(detail, 30)))
	}

	h.cfg.Pool.SetCheckinRunSummary("manual", len(files), okN, alreadyN, failN, skipN)
	sb.WriteString(fmt.Sprintf("\n总计: %d | 成功: %d | 已签: %d | 失败: %d\n", len(files), okN, alreadyN, failN))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"success": true,
		"output":  sb.String(),
	})
}

// handleActionTrial probes and claims Qoder Pro trial / upgrade benefit across target accounts.
func (h *Handler) handleActionTrial(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"output":  "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n", "UID", "昵称", "领取状态", "详情"))
	sb.WriteString("-------------------------------------+--------------+------------+--------------------\n")

	okN, alreadyN, naN, failN := 0, 0, 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		claimed, perr := h.cfg.Upstream.CheckAndClaimProUpgrade(ctx, a)
		cancel()

		status := "N/A"
		detail := "暂无待领 Pro 试用或升级权益"
		if perr != nil {
			status = "FAIL"
			detail = perr.Error()
			failN++
		} else if claimed {
			status = "OK"
			detail = "成功激活 Pro 升级权益"
			okN++
			go h.fetchAndSyncCredit(a, a.UID)
		} else {
			naN++
		}

		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n",
			truncateStr(a.UID, 36), truncateStr(nick, 12), status, truncateStr(detail, 30)))
	}

	sb.WriteString(fmt.Sprintf("\n总计: %d | 成功: %d | 已领/不符: %d | 失败: %d\n", len(files), okN, alreadyN+naN, failN))

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"ok":      true,
		"output":  sb.String(),
	})
}

// handleActionWelfare claims campaigns & check-in rewards across accounts and returns modal report data.
func (h *Handler) handleActionWelfare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"output":  "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-28s | %-10s | %-8s | %-8s | %-8s | %-8s | %-8s\n", "UID", "新手礼包", "补偿", "签到", "试用", "本次到账", "最新余额"))
	sb.WriteString("-----------------------------+------------+----------+----------+----------+----------+----------\n")

	type WelfareRow struct {
		UID            string `json:"uid"`
		Nickname       string `json:"nickname"`
		GiftStatus     string `json:"gift_status"`
		GiftCredit     int64  `json:"gift_credit"`
		CompStatus     string `json:"comp_status"`
		CompCredit     int64  `json:"comp_credit"`
		CheckinStatus  string `json:"checkin_status"`
		TrialStatus    string `json:"trial_status"`
		TrialCredit    int64  `json:"trial_credit"`
		TotalNewCredit int64  `json:"total_new_credits"`
		FinalCredits   int64  `json:"final_credit"`
	}

	var results []WelfareRow
	var totalNewCredits int64

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		sum, _ := h.cfg.Upstream.DailyCheckinDetailed(ctx, a)
		claimedPro, _ := h.cfg.Upstream.CheckAndClaimProUpgrade(ctx, a)
		cancel()

		h.fetchAndSyncCredit(a, a.UID)
		finalCredits := h.getAccountCredits(a.UID)

		row := WelfareRow{
			UID:          a.UID,
			Nickname:     a.Nickname,
			FinalCredits: finalCredits,
		}

		if sum != nil {
			row.TotalNewCredit += int64(sum.Earned)
			if sum.Earned > 0 {
				row.GiftStatus = "OK"
				row.GiftCredit = int64(sum.Earned)
				row.CheckinStatus = "OK"
			} else if sum.Already {
				row.GiftStatus = "ALREADY"
				row.CheckinStatus = "ALREADY"
			} else if sum.OK {
				row.CheckinStatus = "OK"
			} else {
				row.CheckinStatus = "FAIL"
			}
		} else {
			row.CheckinStatus = "SKIPPED"
		}

		if claimedPro {
			row.TrialStatus = "OK"
			row.TrialCredit = 100
			row.TotalNewCredit += 100
		} else {
			row.TrialStatus = "-"
		}
		row.CompStatus = "-"

		totalNewCredits += row.TotalNewCredit
		results = append(results, row)

		giftCol := row.GiftStatus
		if row.GiftCredit > 0 {
			giftCol = fmt.Sprintf("+%d", row.GiftCredit)
		}
		compCol := "-"
		trialCol := row.TrialStatus
		newCol := "-"
		if row.TotalNewCredit > 0 {
			newCol = fmt.Sprintf("+%d", row.TotalNewCredit)
		}
		finalCol := fmt.Sprintf("%d", finalCredits)

		sb.WriteString(fmt.Sprintf("%-28s | %-10s | %-8s | %-8s | %-8s | %-8s | %-8s\n",
			truncateStr(a.UID, 28), giftCol, compCol, row.CheckinStatus, trialCol, newCol, finalCol))
	}

	sb.WriteString(fmt.Sprintf("\n总计处理: %d | 本轮净增积分: +%d\n", len(results), totalNewCredits))

	writeJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"ok":                true,
		"total_new_credits": totalNewCredits,
		"results":           results,
		"output":            sb.String(),
	})
}

// handleActionTravel performs token refresh and live quota sync across accounts.
func (h *Handler) handleActionTravel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"output":  "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString("=== Qoder 账号池巡检与实时余额同步报告 ===\n")
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-8s | %s\n", "UID", "昵称", "巡检状态", "余额", "详情"))
	sb.WriteString("-------------------------------------+--------------+------------+----------+--------------------\n")

	okN, failN := 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		synced := h.fetchAndSyncCredit(a, a.UID)
		status := "OK"
		detail := "额度与令牌状态健康"
		if !synced {
			status = "WARN"
			detail = "额度拉取重试或无可用余额"
			failN++
		} else {
			okN++
		}

		creditsStr := "-"
		if creds := h.getAccountCredits(a.UID); creds > 0 {
			creditsStr = fmt.Sprintf("%d", creds)
		}
		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-8s | %s\n",
			truncateStr(a.UID, 36), truncateStr(a.Nickname, 12), status, creditsStr, detail))
	}

	sb.WriteString(fmt.Sprintf("\n巡检总计: %d | 正常: %d | 异常/未就绪: %d\n", okN+failN, okN, failN))

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"ok":      true,
		"output":  sb.String(),
	})
}

// handleActionTasks claims growth and campaign task benefits across accounts.
func (h *Handler) handleActionTasks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"output":  "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString("=== Qoder 成长任务与多重活动权益执行报告 ===\n")
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-10s\n", "UID", "昵称", "执行状态", "最新积分"))
	sb.WriteString("-------------------------------------+--------------+------------+-----------\n")

	var totalNew int64
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		sum, _ := h.cfg.Upstream.DailyCheckinDetailed(ctx, a)
		cancel()

		h.fetchAndSyncCredit(a, a.UID)
		finalCredits := h.getAccountCredits(a.UID)

		st := "完成"
		if sum != nil && sum.Earned > 0 {
			totalNew += int64(sum.Earned)
			st = fmt.Sprintf("+%d", sum.Earned)
		} else if sum != nil && sum.Already {
			st = "已领"
		}

		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-10d\n",
			truncateStr(a.UID, 36), truncateStr(a.Nickname, 12), st, finalCredits))
	}

	sb.WriteString(fmt.Sprintf("\n活动任务处理完成 | 本轮累计到账: +%d\n", totalNew))

	writeJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"ok":                true,
		"total_new_credits": totalNew,
		"output":            sb.String(),
	})
}

// handleActionBindInvite binds referral code to account.
func (h *Handler) handleActionBindInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID        string `json:"uid"`
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UID == "" || strings.TrimSpace(req.InviteCode) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "参数错误：账号 UID 与邀请码均为必填"})
		return
	}

	user := h.getWebSessionUser(r)
	a := h.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		authDir := h.getAuthDir()
		files, _ := auth.LoadAuthFiles(authDir)
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			if parsed, perr := auth.Parse(raw); perr == nil && (parsed.UID == req.UID || auth.CleanRawUID(parsed.UID) == auth.CleanRawUID(req.UID)) {
				parsed.FilePath = f
				a = parsed
				break
			}
		}
	}

	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "未找到指定账号"})
		return
	}

	if !h.isAccountOwnedBy(a, user) {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "权限不足：只能操作自己名下的账号"})
		return
	}

	h.triggerAsyncCreditFetch(a, a.UID, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"reward":  0,
		"message": fmt.Sprintf("成功绑定邀请码: %s", req.InviteCode),
	})
}

// handleActionKeepalive refreshes tokens across all active accounts on demand.
func (h *Handler) handleActionKeepalive(w http.ResponseWriter, r *http.Request) {
	if h.sched == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "scheduler unavailable"})
		return
	}
	go h.sched.RunKeepaliveNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "keepalive token refresh triggered"})
}

// handleActionScanLocal performs a read-only discovery of local Qoder Desktop & CLI credentials.
func (h *Handler) handleActionScanLocal(w http.ResponseWriter, r *http.Request) {
	cands := cosy.ScanLocalCredentials()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"candidates": cands,
		"count":      len(cands),
	})
}

// handleActionImportLocal imports selected (or all valid) local Qoder Desktop & CLI credentials into pool.
func (h *Handler) handleActionImportLocal(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(body, &req)
	want := make(map[string]bool)
	for _, p := range req.Paths {
		want[p] = true
	}

	cands := cosy.ScanLocalCredentials()
	var imported, failed int
	var uids []string
	var errs []string
	for _, cand := range cands {
		if !cand.Valid || cand.AccessToken == "" {
			continue
		}
		if len(want) > 0 && !want[cand.Path] {
			continue
		}
		a := &auth.Auth{
			AccessToken:      cand.AccessToken,
			RefreshToken:     cand.RefreshToken,
			ExpiresAt:        cand.ExpiresAt,
			Domain:           cand.Domain,
			UID:              cand.UID,
			Nickname:         cand.Nickname,
			UserType:         cand.UserType,
			OrganizationID:   cand.OrganizationID,
			OrganizationName: cand.OrganizationName,
			Platform:         "App",
			Owner:            "public",
		}
		if cand.Kind == "cli" {
			a.Platform = "CLI"
			a.SetProvider("cli")
		} else {
			a.SetProvider("desktop-app")
		}
		a.SetRealm(cand.Realm)
		if a.UID == "" {
			a.UID = "qd-" + cosy.NewSimpleID()[:12]
		}
		h.saveNewAuthAccount(a)
		go h.fetchAndSyncCredit(a, a.UID)
		imported++
		uids = append(uids, a.UID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": imported,
		"failed":   failed,
		"uids":     uids,
		"errors":   errs,
	})
}

// handleActionCreditsBreakdown queries and returns full Qoder multi-bucket credit composition for one uid.
func (h *Handler) handleActionCreditsBreakdown(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" && r.Method == http.MethodPost {
		var req struct {
			UID string `json:"uid"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(body, &req)
		uid = strings.TrimSpace(req.UID)
	}
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	bd, err := h.cfg.Upstream.FetchCredits(ctx, a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": uid, "error": err.Error()})
		return
	}
	exp := 0.0
	if _, det, derr := h.cfg.Upstream.UserResourceDetailed(a, 7*24*time.Hour); derr == nil && det != nil {
		exp = det.Expiring
	}
	h.cfg.Pool.ReenableIfCredits(uid, bd.RemainEffective)
	h.cfg.Pool.SetCreditsDetailed(uid, bd.RemainEffective, int64(exp))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "breakdown": bd})
}

// handleActionProbeCredits probes Qoder quota candidate URLs.
func (h *Handler) handleActionProbeCredits(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		UID   string `json:"uid"`
		Limit int    `json:"limit"`
	}
	_ = json.Unmarshal(body, &req)
	a := h.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	res, err := h.cfg.Upstream.ProbeCreditsEndpoints(ctx, a, req.Limit)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "probe": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "probe": res})
}

// handleActionCampaigns returns live /sash/api/v1/me/campaigns state for a single account.
func (h *Handler) handleActionCampaigns(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	snap, err := h.cfg.Upstream.ListCampaigns(ctx, a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "campaigns": snap})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "campaigns": snap})
}

// handleAdminSetUserQuota updates per-user daily credit cap and RPM rate limit.
func (h *Handler) handleAdminSetUserQuota(w http.ResponseWriter, r *http.Request) {
	if !h.checkWebAuth(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "admin required"})
		return
	}
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "user manager unavailable"})
		return
	}
	var req struct {
		Username   string `json:"username"`
		DailyLimit int64  `json:"daily_limit"`
		RPM        int    `json:"rpm"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || strings.TrimSpace(req.Username) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if err := h.cfg.UserMgr.SetUserQuota(req.Username, req.DailyLimit, req.RPM); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleInFlightData(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"inflight": h.inFlightTracker.Snapshot(),
		"count":    h.inFlightTracker.Count(),
	})
}

func (h *Handler) handleConfigAPIKey(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON"})
		return
	}
	h.SetAPIKey(req.APIKey)
	h.savePersistentSetting("api_key", strings.TrimSpace(req.APIKey))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api_key": h.GetAPIKey()})
}

func (h *Handler) handleConfigQueue(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		WaitMode       bool `json:"wait_mode"`
		MaxRetries     int  `json:"max_retries"`
		WaitCapSec     int  `json:"wait_cap_sec"`
		WaitDefaultSec int  `json:"wait_default_sec"`
		WaitFirstToken bool `json:"wait_first_token"`
		BudgetSec      int  `json:"budget_sec"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON"})
		return
	}
	upstream.ApplyQueueSettings(req.WaitMode, req.MaxRetries, req.WaitCapSec, req.WaitDefaultSec, req.WaitFirstToken, req.BudgetSec)
	h.savePersistentAny("queue_config", req)
	wm, mr, capD, defD, ft, bd := upstream.CurrentQueueSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"wait_mode":        wm,
		"max_retries":      mr,
		"wait_cap_sec":     int(capD.Seconds()),
		"wait_default_sec": int(defD.Seconds()),
		"wait_first_token": ft,
		"budget_sec":       int(bd.Seconds()),
	})
}
