package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/logfmt"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
)

const (
	notFoundCooldown = 30 * time.Second
	wafCooldownBase  = 60 * time.Second
)

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "failed reading request body: "+err.Error())
		return
	}
	reqMap, err := decodeJSONMap(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON: "+err.Error())
		return
	}

	reqModel, _ := reqMap["model"].(string)
	if strings.TrimSpace(reqModel) == "" {
		reqModel = "auto"
		reqMap["model"] = reqModel
	}
	stream, _ := reqMap["stream"].(bool)
	reqOwner := h.requestOwner(r)

	if strings.EqualFold(strings.TrimSpace(reqModel), "dsh") {
		h.forwardToDSH(w, r, body)
		return
	}

	candidates := h.resolveModelCandidates(reqModel)
	rawMsgs, _ := reqMap["messages"].([]any)
	reqHasImage := upstream.RequestHasImages(rawMsgs)

	st := newChatStat(time.Now(), body, stream)
	defer func() { st.done() }()

	convID := deriveConversationID(r, reqMap)
	beginAtMs := time.Now().UnixMilli()

	var lastErr error
	var lastUerr *upstream.Error
	var lastBody []byte
	var lastModel string

	tried := make(map[string]bool)
	retryUsed := 0

	for candIdx, candModel := range candidates {
		realm, modelKey := h.resolveModelForHandler(candModel)
		bareModel := modelKey
		lastModel = bareModel

		if h.modelBreaker != nil && h.modelBreaker.IsCooling(candModel) && h.hasHealthyCandidateAfter(candidates, candIdx, reqModel, reqOwner) {
			continue
		}
		if realm == "global" && !h.cfg.GlobalEnabled {
			if upstream.ExclusiveRealm(modelKey) == "global" {
				st.status = http.StatusBadRequest
				st.errDetail = "global realm disabled"
				writeOpenAIError(w, http.StatusBadRequest, "realm_disabled", "global Qoder realm is disabled in configuration")
				return
			}
			realm = "cn"
		}

		catMeta, inCatalog := h.catalogModelEntry(bareModel, realm)
		if reqHasImage && inCatalog && !catMeta.IsVL {
			if len(candidates) > candIdx+1 {
				continue
			}
			st.status = http.StatusBadRequest
			st.errDetail = "image unsupported by " + bareModel
			writeOpenAIError(w, http.StatusBadRequest, "unsupported_image",
				fmt.Sprintf("model %s does not support images", bareModel),
				upstream.GatewayHint(upstream.ErrBadParams, "image unsupported", upstream.HintContext{
					Model: bareModel, HasImage: true, ModelInCatalog: true, ModelSupportsImages: false,
				}))
			return
		}

		isFreeCall := h.isFreeTierModel(realm, bareModel, candModel)
		triedForCand := make(map[string]bool)

		for turn := 0; turn < h.cfg.MaxRotate; turn++ {
			acct := h.pickAccountForChat(convID, triedForCand, tried, bareModel, realm, reqOwner)
			if acct == nil && h.canFailoverOtherRealm(candModel, reqModel, realm, bareModel) {
				other := "global"
				if realm == "global" {
					other = "cn"
				}
				if other != "global" || h.cfg.GlobalEnabled {
					acct = h.pickAccountForChat(convID, triedForCand, tried, bareModel, other, reqOwner)
					if acct != nil {
						realm = other
					}
				}
			}
			if acct == nil {
				break
			}
			triedForCand[acct.UID] = true
			tried[acct.UID] = true

			if h.wafIP.active() {
				st.status = http.StatusTooManyRequests
				st.errDetail = "waf ip fail-fast"
				writeOpenAIError(w, http.StatusTooManyRequests, "waf_blocked",
					"gateway temporarily paused after repeated upstream WAF challenges",
					upstream.GatewayHint(upstream.ErrWafBlock, "", upstream.HintContext{Model: bareModel}))
				return
			}

			if !h.cfg.Pool.Acquire(acct.UID) {
				if !sleepCtx(r.Context(), backoffAfter(retryUsed)) {
					break
				}
				retryUsed++
				continue
			}

			st.uid = acct.UID
			st.nick = acct.Nickname

			reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
			endInFlight := h.inFlightTracker.Begin(reqID, bareModel, acct.UID, acct.Nickname, stream, upstream.ExtractClientIP(r), reqOwner)
			cleanupAttempt := func() {
				if endInFlight != nil {
					endInFlight()
					endInFlight = nil
				}
				h.cfg.Pool.Release(acct.UID)
			}

			atVal := strings.ToLower(acct.AccessTokenValue())
			if strings.HasPrefix(atVal, "http://") || strings.HasPrefix(atVal, "https://") {
				log.Printf("ERR: [server] 账号 %s 凭证无效: AccessToken 误存为了网页授权链接", logfmt.Label(acct.UID, acct.Nickname))
				h.cfg.Pool.NoteSessionDead(acct.UID)
				cleanupAttempt()
				lastErr = fmt.Errorf("账号 %s 凭证无效: AccessToken 误存为了网页授权链接", acct.UID)
				continue
			}

			if acct.NeedsRefresh(h.cfg.RefreshSkew) && (acct.RefreshTokenValue() != "" || acct.PersonalTokenValue() != "") {
				if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
					log.Printf("WARN: [server] refresh failed acct=%s err=%v", logfmt.Label(acct.UID, acct.Nickname), rerr)
					var ue *upstream.Error
					if errors.As(rerr, &ue) && ue.Kind == upstream.ErrSessionDead {
						h.cfg.Pool.NoteSessionDead(acct.UID)
					} else {
						h.cfg.Pool.NoteError(acct.UID)
					}
					cleanupAttempt()
					lastErr = rerr
					continue
				}
			}

			var clientIP string
			if h.cfg.Upstream.PassthroughIP {
				clientIP = upstream.ExtractClientIP(r)
			}

			chatMeta := upstream.ChatMeta{
				ConversationID: convID,
				BeginAtMs:      beginAtMs,
				PromptMode:     h.cfg.PromptMode,
				SystemPrompt:   h.cfg.SystemPrompt,
			}

			sseHeadersSent := false
			flusher, _ := w.(http.Flusher)
			var queueCB upstream.QueueWaitCallback
			if stream {
				queueCB = func(wait time.Duration, round int, queueCount int) {
					if !sseHeadersSent {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Cache-Control", "no-cache")
						w.Header().Set("Connection", "keep-alive")
						w.Header().Set("X-Accel-Buffering", "no")
						w.WriteHeader(http.StatusOK)
						sseHeadersSent = true
					}
					_, _ = fmt.Fprintf(w, ": ping (queued round=%d count=%d wait=%ds)\n\n", round, queueCount, int(wait.Seconds()))
					if flusher != nil {
						flusher.Flush()
					}
				}
			}

			rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, reqMap, modelKey, chatMeta, clientIP, queueCB)

			if terr != nil && (status == http.StatusUnauthorized || (errors.As(terr, &lastUerr) && lastUerr.Kind == upstream.ErrSessionDead)) &&
				!sseHeadersSent && (acct.RefreshTokenValue() != "" || acct.PersonalTokenValue() != "") {
				if rerr := h.cfg.Upstream.RefreshToken(acct); rerr == nil {
					rc, status, respBody, terr = h.cfg.Upstream.ChatStreamContext(r.Context(), acct, reqMap, modelKey, chatMeta, clientIP, queueCB)
				}
			}

			if terr == nil && rc != nil {
				if convID != "" && h.cfg.Sessions != nil {
					h.cfg.Sessions.Bind(convID, acct.UID)
				}
				h.cfg.Pool.NoteSuccess(acct.UID)
				h.cfg.Pool.ClearSessionDead(acct.UID)
				if h.modelBreaker != nil {
					h.modelBreaker.NoteSuccess(candModel)
				}

				st.uid = acct.UID
				st.nick = acct.Nickname
				st.status = http.StatusOK

				if stream {
					if !sseHeadersSent {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Cache-Control", "no-cache")
						w.Header().Set("Connection", "keep-alive")
						w.Header().Set("X-Accel-Buffering", "no")
						w.WriteHeader(http.StatusOK)
						sseHeadersSent = true
					}
					stats := &upstream.StreamStats{}
					sErr := upstream.StreamHint(w, rc, stats, upstream.FrameHintFunc(func() upstream.HintContext {
						return h.hintContext(bareModel, reqHasImage, realm)
					}))
					_ = rc.Close()
					cleanupAttempt()

					st.hasUsage = stats.PromptTokens > 0 || stats.CompletionTokens > 0
					st.prompt = int(stats.PromptTokens)
					st.toks = int(stats.CompletionTokens)
					if !st.hasUsage {
						st.toks = -1
					}
					st.cacheHit = int(stats.CachedTokens)
					if !stats.FirstByteAt.IsZero() {
						st.ttfb = stats.FirstByteAt.Sub(st.start)
					}
					h.recordModelCost(acct.UID, bareModel, isFreeCall, stats.PromptTokens+stats.CompletionTokens, catMeta.PriceFactor)

					if upstream.IsEmptyStreamError(sErr) {
						h.cfg.Pool.NoteFailures(acct.UID)
						st.status = http.StatusBadGateway
						st.errDetail = "empty upstream stream"
					}
					return
				}

				agg, aerr := upstream.Aggregate(rc)
				_ = rc.Close()
				cleanupAttempt()
				if aerr != nil {
					lastErr = aerr
					st.status = http.StatusBadGateway
					st.errDetail = aerr.Error()
					writeOpenAIError(w, http.StatusBadGateway, "upstream_error", aerr.Error())
					return
				}
				fillStatFromUsage(st, agg)
				if ct := completionTokens(agg); ct >= 0 {
					st.toks = ct
				}
				h.recordModelCost(acct.UID, bareModel, isFreeCall, int64(st.prompt+max(0, st.toks)), catMeta.PriceFactor)
				writeJSON(w, http.StatusOK, agg)
				return
			}

			cleanupAttempt()
			var uerr *upstream.Error
			errors.As(terr, &uerr)
			if uerr == nil && terr != nil {
				if errors.Is(terr, context.Canceled) || r.Context().Err() != nil {
					st.status = 499
					st.errDetail = "client disconnected"
					return
				}
				uerr = &upstream.Error{Kind: upstream.ErrServer, Status: status, Msg: terr.Error()}
			}
			lastErr = terr
			lastUerr = uerr
			lastBody = respBody

			if sseHeadersSent {
				hint := h.hintOf(uerr.Kind, uerr.Msg, bareModel, reqHasImage, realm)
				errChunk, _ := encodeJSONBytes(map[string]any{
					"error": map[string]any{
						"message":      uerr.Msg,
						"type":         string(uerr.Kind),
						"code":         uerr.Code,
						"gateway_hint": hint,
					},
				})
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", string(errChunk))
				if flusher != nil {
					flusher.Flush()
				}
				st.status = status
				st.errDetail = uerr.Msg
				return
			}

			h.applyErrorPolicy(acct.UID, uerr.Kind, uerr.Msg, bareModel, uerr)

			// If WAF IP-level fail-fast just triggered, stop rotating remaining accounts immediately
			if uerr.Kind == upstream.ErrWafBlock && h.wafIP.active() {
				if strings.TrimSpace(lastUerr.Msg) == "" {
					lastUerr.Msg = "waf ip-level block active, fail-fast stopped rotation"
				}
				break
			}

			// Deterministic request-level failures never benefit from rotating other accounts
			if uerr.Kind == upstream.ErrContentBlocked || uerr.Kind == upstream.ErrPromptTooLong ||
				uerr.Kind == upstream.ErrImageInvalid || uerr.Kind == upstream.ErrBadParams {
				break
			}
			if uerr.Kind == upstream.ErrModelBlocked || uerr.Kind == upstream.ErrQueueWait {
				if h.modelBreaker != nil && uerr.Kind == upstream.ErrModelBlocked {
					h.modelBreaker.Trip(candModel, 2*time.Minute)
				}
				break
			}
			retryUsed++
			if !sleepCtx(r.Context(), backoffAfter(retryUsed-1)) {
				break
			}
		}

		if lastUerr != nil && (lastUerr.Kind == upstream.ErrWafBlock && h.wafIP.active() ||
			lastUerr.Kind == upstream.ErrContentBlocked || lastUerr.Kind == upstream.ErrPromptTooLong ||
			lastUerr.Kind == upstream.ErrImageInvalid || lastUerr.Kind == upstream.ErrBadParams) {
			break
		}
	}

	if lastUerr != nil {
		httpStatus := http.StatusServiceUnavailable
		switch lastUerr.Kind {
		case upstream.ErrPromptTooLong:
			httpStatus = http.StatusRequestEntityTooLarge
		case upstream.ErrContentBlocked, upstream.ErrBadParams, upstream.ErrImageInvalid, upstream.ErrClient:
			httpStatus = http.StatusBadRequest
		}
		st.status = httpStatus
		msg := strings.TrimSpace(lastUerr.Msg)
		if msg == "" {
			msg = strings.TrimSpace(string(lastBody))
		}
		if msg == "" && lastUerr.Kind == upstream.ErrWafBlock {
			msg = "waf ip-level block: upstream HTTP 403"
		}
		st.errDetail = msg
		writeOpenAIError(w, httpStatus, string(lastUerr.Kind), msg, h.hintOf(lastUerr.Kind, msg, lastModel, reqHasImage, "cn"))
		return
	}

	st.status = http.StatusServiceUnavailable
	errMsg := "no healthy account available in pool"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	st.errDetail = errMsg
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", errMsg, upstream.NoHealthyAccountHint())
}

func (h *Handler) pickAccountForChat(convID string, triedForCand, globalTried map[string]bool, bareModel, realm, owner string) *auth.Auth {
	if h == nil || h.cfg.Pool == nil {
		return nil
	}
	combined := make(map[string]bool, len(triedForCand)+len(globalTried))
	for k, v := range globalTried {
		combined[k] = v
	}
	for k, v := range triedForCand {
		combined[k] = v
	}
	if convID != "" && h.cfg.Sessions != nil {
		if stickyUID, ok := h.cfg.Sessions.ResolveForModel(convID, bareModel); ok && stickyUID != "" && !combined[stickyUID] {
			if acct := h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel); acct != nil && acct.Realm() == realm {
				if owner == "public" || acct.OwnerValue() == owner || acct.OwnerValue() == "public" {
					return acct
				}
			}
		}
	}
	return h.cfg.Pool.PickExcludingForRealmAndOwner(combined, bareModel, realm, owner)
}

func (h *Handler) canFailoverOtherRealm(candModel, reqModel, realm, bareModel string) bool {
	if h == nil || h.cfg.Pool == nil {
		return false
	}
	totalInRealm, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
	return canCandidateCrossFailover(candModel, reqModel, realm, bareModel, totalInRealm)
}

func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	if h == nil || h.cfg.Pool == nil || uid == "" {
		return
	}
	switch kind {
	case upstream.ErrHardCredit:
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "insufficient credits")
	case upstream.ErrQueueWait:
		// 10605 queue is model capacity contention; do not penalize account health.
	case upstream.ErrSoftRate:
		isModelLimit := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if isModelLimit {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "model rate/quota limit")
			} else {
				h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, resetAt, "rate limit")
			}
			return
		}
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Now().Add(uerr.RetryAfter), "rate limit (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		h.wafIP.noteWaf(uid)
		dur := jitterDur(wafCooldownBase)
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, dur, time.Now().Add(uerr.RetryAfter), "waf 403 block")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, dur, time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		if h.cfg.Pool.NoteSessionDead(uid) {
			if h.alertMgr != nil {
				nick := uid
				if a := h.cfg.Pool.AuthByUID(uid); a != nil {
					nick = a.Nickname
				}
				h.alertMgr.NotifyBan(uid, nick, "TOKEN_EXPIRE / session dead")
			}
		}
	case upstream.ErrModelBlocked:
		if model != "" {
			h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
		}
	case upstream.ErrAccountFault:
		if strings.Contains(strings.ToLower(body), "request illegal") {
			reason := "account banned by upstream, re-login required"
			h.cfg.Pool.Disable(uid, reason)
			if h.alertMgr != nil {
				nick := uid
				if a := h.cfg.Pool.AuthByUID(uid); a != nil {
					nick = a.Nickname
				}
				h.alertMgr.NotifyBan(uid, nick, reason)
			}
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault")
	case upstream.ErrNotFound:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		h.cfg.Pool.NoteError(uid)
	}
}

func (h *Handler) recordModelCost(uid, model string, isFree bool, tokens int64, priceFactor float64) {
	if h == nil || h.cfg.Pool == nil || uid == "" || model == "" {
		return
	}
	if isFree || priceFactor <= 0 {
		h.cfg.Pool.NoteModelCost(uid, model, 0, int(tokens))
		return
	}
	h.cfg.Pool.NoteModelCost(uid, model, priceFactor, int(tokens))
}

func (h *Handler) hintContext(bareModel string, hasImage bool, realm string) upstream.HintContext {
	meta, inCat := h.catalogModelEntry(bareModel, realm)
	return upstream.HintContext{
		Model:               bareModel,
		HasImage:            hasImage,
		ModelSupportsImages: meta.IsVL,
		ModelInCatalog:      inCat,
	}
}

func (h *Handler) hintOf(kind upstream.ErrKind, msg, bareModel string, hasImage bool, realm string) string {
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage, realm))
}

func (h *Handler) forwardToDSH(w http.ResponseWriter, r *http.Request, body []byte) {
	if h.cfg.DshMgr == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "dsh_unavailable", "DSH manager not initialized")
		return
	}
	st := h.cfg.DshMgr.GetStatus()
	if !st.Running || st.Port <= 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "dsh_stopped", "DeepSeek Harness (dsh) service is not running")
		return
	}
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", st.Port)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "dsh_proxy_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if authz := r.Header.Get("Authorization"); authz != "" {
		req.Header.Set("Authorization", authz)
	} else if ak := h.GetAPIKey(); ak != "" {
		req.Header.Set("Authorization", "Bearer "+ak)
	}
	resp, err := (&http.Client{Timeout: 600 * time.Second}).Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "dsh_upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func deriveConversationID(r *http.Request, reqMap map[string]any) string {
	for _, hdr := range []string{"X-Conversation-ID", "X-Session-ID", "Conversation-Id"} {
		if v := strings.TrimSpace(r.Header.Get(hdr)); v != "" {
			return v
		}
	}
	if md, ok := reqMap["metadata"].(map[string]any); ok {
		if uid, ok := md["user_id"].(string); ok && strings.TrimSpace(uid) != "" {
			return strings.TrimSpace(uid)
		}
	}
	if u, ok := reqMap["user"].(string); ok && strings.TrimSpace(u) != "" {
		return strings.TrimSpace(u)
	}
	if msgs, ok := reqMap["messages"].([]any); ok && len(msgs) > 0 {
		for _, raw := range msgs {
			if m, ok := raw.(map[string]any); ok && strings.EqualFold(fmt.Sprint(m["role"]), "user") {
				txt, _, _ := upstream.FlattenContent(m["content"])
				if txt != "" {
					if len(txt) > 128 {
						txt = txt[:128]
					}
					return "pfx:" + txt
				}
			}
		}
	}
	return ""
}
