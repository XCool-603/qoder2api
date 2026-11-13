package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
	"qoder2api/internal/dshmgr"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
	"qoder2api/internal/usermgr"
)

var (
	creditFetchMu  sync.Mutex
	creditFetching = make(map[string]time.Time)

	creditDetailMu    sync.RWMutex
	creditDetailCache = make(map[string]CreditBreakdown)
)

// CreditPackDetail represents one granular package/bucket quota slice for an account.
type CreditPackDetail struct {
	PackageCode    string  `json:"package_code,omitempty"`
	PackageName    string  `json:"package_name"`
	SubProductCode string  `json:"sub_product_code,omitempty"`
	Category       string  `json:"category"` // "base" | "bonus" | "purchased"
	Status         int     `json:"status"`
	Remain         float64 `json:"remain"`
	Total          float64 `json:"total"`
	Used           float64 `json:"used"`
	CycleStartTime string  `json:"cycle_start_time,omitempty"`
	CycleEndTime   string  `json:"cycle_end_time,omitempty"`
	IsActive       bool    `json:"is_active"`
	IsExpiringSoon bool    `json:"is_expiring_soon,omitempty"`
}

// CreditBreakdown contains the full credit structure and package list for an account.
type CreditBreakdown struct {
	Remain          float64            `json:"remain"`
	Total           float64            `json:"total"`
	Used            float64            `json:"used"`
	ActiveTotal     float64            `json:"active_total"`
	BaseRemain      float64            `json:"base_remain"`
	BonusRemain     float64            `json:"bonus_remain"`
	PurchasedRemain float64            `json:"purchased_remain"`
	ExpiringRemain  float64            `json:"expiring_remain"`
	TotalDosage     int64              `json:"total_dosage,omitempty"`
	PackCount       int                `json:"pack_count"`
	ActivePackCount int                `json:"active_pack_count"`
	Packs           []CreditPackDetail `json:"packs,omitempty"`
}

func getCachedCreditBreakdown(uid string) *CreditBreakdown {
	creditDetailMu.RLock()
	defer creditDetailMu.RUnlock()
	bd, ok := creditDetailCache[uid]
	if !ok {
		return nil
	}
	cp := bd
	if len(bd.Packs) > 0 {
		cp.Packs = append([]CreditPackDetail(nil), bd.Packs...)
	}
	return &cp
}

// ModelQuotaSummary aggregates per-model rate limiting & observed cost in realm.
type ModelQuotaSummary struct {
	Model        string  `json:"model"`
	LimitedCount int     `json:"limited_count"`
	Reason       string  `json:"reason,omitempty"`
	CostPer1k    float64 `json:"cost_per_1k,omitempty"`
	Samples      int     `json:"samples,omitempty"`
}

// RealmCreditSummary aggregates pool credits & health across All / CN / Global views.
type RealmCreditSummary struct {
	Remain                 float64             `json:"remain"`
	Total                  float64             `json:"total"`
	Used                   float64             `json:"used"`
	ActiveTotal            float64             `json:"active_total"`
	BaseRemain             float64             `json:"base_remain"`
	BonusRemain            float64             `json:"bonus_remain"`
	PurchasedRemain        float64             `json:"purchased_remain"`
	ExpiringRemain         float64             `json:"expiring_remain"`
	PackCount              int                 `json:"pack_count"`
	ActivePackCount        int                 `json:"active_pack_count"`
	AccountCount           int                 `json:"account_count"`
	HealthyCount           int                 `json:"healthy_count"`
	CoolingCount           int                 `json:"cooling_count"`
	DisabledCount          int                 `json:"disabled_count"`
	DetailAccountCount     int                 `json:"detail_account_count"`
	RateLimitedModelsCount int                 `json:"rate_limited_models_count"`
	RateLimitedModels      []string            `json:"rate_limited_models,omitempty"`
	ModelQuotas            []ModelQuotaSummary `json:"model_quotas,omitempty"`
}

// CreditSummaryPayload summarizes credits across All, CN, and Global scopes.
type CreditSummaryPayload struct {
	All    RealmCreditSummary `json:"all"`
	CN     RealmCreditSummary `json:"cn"`
	Global RealmCreditSummary `json:"global"`
}

type realmQuotaAcc struct {
	summary    RealmCreditSummary
	limitedMap map[string]int
	reasonMap  map[string]string
	costSumMap map[string]float64
	costWgtMap map[string]int
	samplesMap map[string]int
}

func newRealmQuotaAcc() *realmQuotaAcc {
	return &realmQuotaAcc{
		limitedMap: make(map[string]int),
		reasonMap:  make(map[string]string),
		costSumMap: make(map[string]float64),
		costWgtMap: make(map[string]int),
		samplesMap: make(map[string]int),
	}
}

func roundSummary2(v float64) float64 {
	return math.Round(v*100) / 100
}

func (a *realmQuotaAcc) add(disabled, manualDisabled, cooling bool, credits, expiring int64, bd *CreditBreakdown, rl []pool.RateLimitedModel, costs []pool.ModelCostStatus) {
	a.summary.AccountCount++
	if disabled || manualDisabled {
		a.summary.DisabledCount++
	} else if cooling {
		a.summary.CoolingCount++
	} else {
		a.summary.HealthyCount++
	}

	if bd != nil {
		a.summary.DetailAccountCount++
		a.summary.Remain += bd.Remain
		a.summary.Total += bd.Total
		a.summary.Used += bd.Used
		a.summary.ActiveTotal += bd.ActiveTotal
		a.summary.BaseRemain += bd.BaseRemain
		a.summary.BonusRemain += bd.BonusRemain
		a.summary.PurchasedRemain += bd.PurchasedRemain
		a.summary.ExpiringRemain += bd.ExpiringRemain
		a.summary.PackCount += bd.PackCount
		a.summary.ActivePackCount += bd.ActivePackCount
	} else if credits >= 0 {
		a.summary.Remain += float64(credits)
		if expiring > 0 {
			a.summary.ExpiringRemain += float64(expiring)
		}
	}

	for _, rm := range rl {
		m := strings.TrimSpace(rm.Model)
		if m == "" {
			continue
		}
		a.summary.RateLimitedModelsCount++
		a.limitedMap[m]++
		if a.reasonMap[m] == "" && rm.Reason != "" {
			a.reasonMap[m] = rm.Reason
		}
	}
	for _, mc := range costs {
		m := strings.TrimSpace(mc.Model)
		if m == "" {
			continue
		}
		w := mc.Samples
		if w <= 0 {
			w = 1
		}
		a.costSumMap[m] += mc.CostPer1k * float64(w)
		a.costWgtMap[m] += w
		a.samplesMap[m] += mc.Samples
	}
}

func (a *realmQuotaAcc) finalize() RealmCreditSummary {
	out := a.summary
	out.Remain = roundSummary2(out.Remain)
	out.Total = roundSummary2(out.Total)
	out.Used = roundSummary2(out.Used)
	out.ActiveTotal = roundSummary2(out.ActiveTotal)
	out.BaseRemain = roundSummary2(out.BaseRemain)
	out.BonusRemain = roundSummary2(out.BonusRemain)
	out.PurchasedRemain = roundSummary2(out.PurchasedRemain)
	out.ExpiringRemain = roundSummary2(out.ExpiringRemain)

	modelSet := make(map[string]struct{})
	for m := range a.limitedMap {
		modelSet[m] = struct{}{}
	}
	for m := range a.costWgtMap {
		modelSet[m] = struct{}{}
	}
	if len(a.limitedMap) > 0 {
		limitedNames := make([]string, 0, len(a.limitedMap))
		for m := range a.limitedMap {
			limitedNames = append(limitedNames, m)
		}
		sort.Strings(limitedNames)
		out.RateLimitedModels = make([]string, 0, len(limitedNames))
		for _, m := range limitedNames {
			if cnt := a.limitedMap[m]; cnt > 1 {
				out.RateLimitedModels = append(out.RateLimitedModels, fmt.Sprintf("%s (×%d)", m, cnt))
			} else {
				out.RateLimitedModels = append(out.RateLimitedModels, m)
			}
		}
	}
	if len(modelSet) > 0 {
		names := make([]string, 0, len(modelSet))
		for m := range modelSet {
			names = append(names, m)
		}
		sort.Strings(names)
		out.ModelQuotas = make([]ModelQuotaSummary, 0, len(names))
		for _, m := range names {
			avgCost := 0.0
			if w := a.costWgtMap[m]; w > 0 {
				avgCost = roundSummary2(a.costSumMap[m] / float64(w))
			}
			out.ModelQuotas = append(out.ModelQuotas, ModelQuotaSummary{
				Model:        m,
				LimitedCount: a.limitedMap[m],
				Reason:       a.reasonMap[m],
				CostPer1k:    avgCost,
				Samples:      a.samplesMap[m],
			})
		}
	}
	return out
}

type creditSummaryBuilder struct {
	all    *realmQuotaAcc
	cn     *realmQuotaAcc
	global *realmQuotaAcc
}

func newCreditSummaryBuilder() *creditSummaryBuilder {
	return &creditSummaryBuilder{
		all:    newRealmQuotaAcc(),
		cn:     newRealmQuotaAcc(),
		global: newRealmQuotaAcc(),
	}
}

func (b *creditSummaryBuilder) add(realm string, disabled, manualDisabled, cooling bool, credits, expiring int64, bd *CreditBreakdown, rl []pool.RateLimitedModel, costs []pool.ModelCostStatus) {
	b.all.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "cn":
		b.cn.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	case "global":
		b.global.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	}
}

func (b *creditSummaryBuilder) build() CreditSummaryPayload {
	return CreditSummaryPayload{
		All:    b.all.finalize(),
		CN:     b.cn.finalize(),
		Global: b.global.finalize(),
	}
}

// CheckinSummaryPayload contains daily CST auto-checkin schedule and summary.
type CheckinSummaryPayload struct {
	Enabled      bool   `json:"enabled"`
	Hours        []int  `json:"hours"`
	LastRunAt    string `json:"last_run_at,omitempty"`
	NextRunAt    string `json:"next_run_at,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
	TodayChecked int    `json:"today_checked"`
	CNAccounts   int    `json:"cn_accounts"`
	OK           int    `json:"ok"`
	Already      int    `json:"already"`
	Fail         int    `json:"fail"`
	Skipped      int    `json:"skipped"`
}

func (h *Handler) buildCheckinSummary(todayChecked, cnAccounts int) CheckinSummaryPayload {
	cst := time.FixedZone("CST", 8*3600)
	enabled := true
	if h.cfg.CheckinEnabled != nil {
		enabled = *h.cfg.CheckinEnabled
	}
	hours := h.cfg.CheckinHours
	if len(hours) == 0 {
		hours = []int{9, 21}
	}
	out := CheckinSummaryPayload{
		Enabled:      enabled,
		Hours:        append([]int(nil), hours...),
		TodayChecked: todayChecked,
		CNAccounts:   cnAccounts,
	}
	if enabled {
		nowCST := time.Now().In(cst)
		var earliest time.Time
		for _, hr := range hours {
			if hr < 0 || hr > 23 {
				continue
			}
			t := time.Date(nowCST.Year(), nowCST.Month(), nowCST.Day(), hr, 0, 0, 0, cst)
			if !t.After(nowCST) {
				t = t.Add(24 * time.Hour)
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
		if !earliest.IsZero() {
			out.NextRunAt = earliest.Format("15:04")
		}
	}
	if h.cfg.Pool != nil {
		if sum := h.cfg.Pool.GetCheckinRunSummary(); sum != nil && !sum.LastRunAt.IsZero() {
			out.LastRunAt = sum.LastRunAt.In(cst).Format("01-02 15:04")
			out.Trigger = sum.Trigger
			out.OK = sum.OK
			out.Already = sum.Already
			out.Fail = sum.Fail
			out.Skipped = sum.Skipped
		}
	}
	return out
}

func (h *Handler) fetchAndSyncCredit(a *auth.Auth, uid string) bool {
	if h == nil || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return false
	}
	if live := h.cfg.Pool.AuthByUID(uid); live != nil {
		a = live
	}
	if a == nil || a.AccessTokenValue() == "" {
		return false
	}
	if a.NeedsRefresh(2 * 3600) {
		if err := h.cfg.Upstream.RefreshToken(a); err == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	qb, err := h.cfg.Upstream.FetchCredits(ctx, a)
	if err != nil && a.RefreshTokenValue() != "" {
		if rerr := h.cfg.Upstream.RefreshToken(a); rerr == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
			qb, err = h.cfg.Upstream.FetchCredits(ctx, a)
		}
	}

	var bd CreditBreakdown
	if qb != nil && (qb.Remain > 0 || len(qb.Packages) > 0 || qb.Size > 0) {
		bd = CreditBreakdown{
			Remain:          float64(qb.Remain),
			Total:           float64(qb.Size),
			Used:            float64(qb.Used),
			ActiveTotal:     float64(qb.RemainEffective),
			ExpiringRemain:  float64(qb.ExpiringSoon),
			TotalDosage:     qb.Used,
		}
		for _, pk := range qb.Packages {
			cat := "bonus"
			pkLower := strings.ToLower(pk.Name)
			if pk.Kind == "plan" || strings.Contains(pkLower, "free") || strings.Contains(pkLower, "plan") || strings.Contains(pkLower, "基础") {
				cat = "base"
				bd.BaseRemain += pk.Remain
			} else if pk.Kind == "pack" || pk.Kind == "addon" || strings.Contains(pkLower, "购") {
				cat = "purchased"
				bd.PurchasedRemain += pk.Remain
			} else {
				bd.BonusRemain += pk.Remain
			}
			isExpSoon := pk.DaysLeft <= 7 && pk.DaysLeft >= 0
			bd.Packs = append(bd.Packs, CreditPackDetail{
				PackageName:    pk.Name,
				Category:       cat,
				Remain:         pk.Remain,
				Total:          pk.Size,
				Used:           pk.Used,
				CycleEndTime:   pk.ExpireDate,
				IsActive:       pk.Available,
				IsExpiringSoon: isExpSoon,
			})
		}
		if qb.Shared != nil {
			bd.Packs = append(bd.Packs, CreditPackDetail{
				PackageName:  qb.Shared.Name,
				Category:     "bonus",
				Remain:       qb.Shared.Remain,
				Total:        qb.Shared.Size,
				Used:         qb.Shared.Used,
				CycleEndTime: qb.Shared.ExpireDate,
				IsActive:     qb.Shared.Available,
			})
		}
		bd.PackCount = len(bd.Packs)
		for _, p := range bd.Packs {
			if p.IsActive {
				bd.ActivePackCount++
			}
		}
	} else {
		// Fallback to UserResourceDetailed
		rem, detail, derr := h.cfg.Upstream.UserResourceDetailed(a, 7*24*time.Hour)
		if derr != nil && err != nil {
			return false
		}
		exp := 0.0
		if detail != nil {
			exp = detail.Expiring
		}
		bd = CreditBreakdown{
			Remain:         rem,
			Total:          rem,
			ActiveTotal:    rem,
			BaseRemain:     rem,
			ExpiringRemain: exp,
			PackCount:      1,
			ActivePackCount: 1,
			Packs: []CreditPackDetail{
				{
					PackageName:    "Qoder 账户算力额度",
					Category:       "base",
					Remain:         rem,
					Total:          rem,
					IsActive:       rem > 0,
					IsExpiringSoon: exp > 0,
				},
			},
		}
		if detail != nil && len(detail.Buckets) > 0 {
			bd.Packs = nil
			for _, b := range detail.Buckets {
				cat := "bonus"
				if b.Kind == "plan" || strings.Contains(strings.ToLower(b.Name), "free") {
					cat = "base"
				} else if b.Kind == "pack" {
					cat = "purchased"
				}
				bd.Packs = append(bd.Packs, CreditPackDetail{
					PackageName:    b.Name,
					Category:       cat,
					Remain:         b.Remain,
					Total:          b.Size,
					Used:           b.Used,
					CycleEndTime:   b.ExpireDate,
					IsActive:       b.Available,
					IsExpiringSoon: b.DaysLeft <= 7 && b.DaysLeft >= 0,
				})
			}
			bd.PackCount = len(bd.Packs)
			bd.ActivePackCount = 0
			for _, p := range bd.Packs {
				if p.IsActive {
					bd.ActivePackCount++
				}
			}
		}
	}

	creditDetailMu.Lock()
	creditDetailCache[uid] = bd
	creditDetailMu.Unlock()

	remain := int64(bd.Remain)
	h.cfg.Pool.SetCreditsDetailed(uid, remain, int64(bd.ExpiringRemain))
	h.cfg.Pool.ReenableIfCredits(uid, remain)
	if h.alertMgr != nil && remain < int64(h.alertMgr.cfg.MinCredit) {
		h.alertMgr.NotifyLowCredit(uid, a.Nickname, remain)
	}
	return true
}

func (h *Handler) syncPoolCreditsWithTimeout(timeout time.Duration) {
	if h == nil || h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return
	}
	var wg sync.WaitGroup
	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		wg.Add(1)
		go func(authObj *auth.Auth, uid string) {
			defer wg.Done()
			h.fetchAndSyncCredit(authObj, uid)
		}(a, st.UID)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (h *Handler) triggerAsyncCreditFetch(a *auth.Auth, uid string, force bool) {
	if a == nil || a.AccessTokenValue() == "" || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return
	}
	creditFetchMu.Lock()
	last, fetching := creditFetching[uid]
	if !force && fetching && time.Since(last) < 15*time.Second {
		creditFetchMu.Unlock()
		return
	}
	creditFetching[uid] = time.Now()
	creditFetchMu.Unlock()

	go func() {
		h.fetchAndSyncCredit(a, uid)
	}()
}

func (h *Handler) isAccountOwnedBy(a *auth.Auth, user *usermgr.User) bool {
	if a == nil {
		return false
	}
	if user == nil || user.Role == usermgr.RoleAdmin || user.Username == "admin" || user.ID == "u_admin" || user.ID == "admin" {
		return true
	}
	owner := a.OwnerValue()
	return owner == user.ID || owner == user.Username
}

func (h *Handler) isAccountVisibleTo(a *auth.Auth, user *usermgr.User) bool {
	if a == nil {
		return false
	}
	if user == nil || user.Role == usermgr.RoleAdmin || user.Username == "admin" || user.ID == "u_admin" || user.ID == "admin" {
		return true
	}
	owner := a.OwnerValue()
	if owner == user.ID || owner == user.Username {
		return true
	}
	return owner == "" || owner == "public" || owner == "admin" || owner == "u_admin"
}

func defaultInviteCode(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	code := auth.CleanRawUID(a.RawUID())
	if code == "" {
		code = auth.CleanRawUID(a.UID)
	}
	if code == "" {
		code = a.UID
	}
	return code
}

func defaultInviteURL(a *auth.Auth, code string) string {
	if code == "" {
		return ""
	}
	if a != nil && a.IsGlobal() {
		return "https://qoder.com/i/" + code
	}
	return "https://qoder.com.cn/i/" + code
}

func (h *Handler) modelList() []map[string]any {
	var infos []upstream.ModelInfo
	if h != nil && h.cfg.Upstream != nil {
		infos = h.cfg.Upstream.MergedModelInfos(true, h.cfg.GlobalEnabled)
	}
	dshRunning := false
	if h != nil && h.cfg.DshMgr != nil {
		dshRunning = h.cfg.DshMgr.GetStatus().Running
	}
	return buildModelListEntries(infos, true, dshRunning, h)
}

// RegisterWebUI mounts the public portal, admin console, OAuth, batch import, DSH, and RBAC routes.
func (h *Handler) RegisterWebUI() {
	// 前台炫酷门户展示页 (公开展示)
	h.mux.HandleFunc("GET /{$}", h.handlePortalHTML)
	// 公共网关健康与模型状态 (无需鉴权，脱敏安全)
	h.mux.HandleFunc("GET /ui/public/status", h.handlePublicStatus)

	// 管理控制台入口
	h.mux.HandleFunc("GET /console", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /console/", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /dashboard", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /dashboard/", h.handleDashboardHTML)

	// Web Session Auth & Registration
	h.mux.HandleFunc("POST /ui/auth/login", h.handleWebLogin)
	h.mux.HandleFunc("POST /ui/auth/logout", h.handleWebLogout)
	h.mux.HandleFunc("POST /ui/auth/register", h.handleWebRegister)

	// 控制台数据与账号池
	h.mux.HandleFunc("GET /ui/data", h.withWebAuth(h.handleDashboardData))
	h.mux.HandleFunc("POST /ui/oauth/start", h.withWebAuth(h.handleOAuthStart))
	h.mux.HandleFunc("GET /ui/oauth/poll", h.withWebAuth(h.handleOAuthPoll))
	h.mux.HandleFunc("POST /ui/oauth/poll", h.withWebAuth(h.handleOAuthPoll))
	h.mux.HandleFunc("POST /ui/oauth/cancel", h.withWebAuth(h.handleOAuthCancel))
	h.mux.HandleFunc("POST /ui/oauth/import", h.withWebAuth(h.handleOAuthImport))
	h.mux.HandleFunc("POST /ui/oauth/batch_import", h.withWebAuth(h.handleOAuthBatchImport))
	h.mux.HandleFunc("POST /ui/backup/import", h.withWebAuth(h.handleOAuthBatchImport))
	h.mux.HandleFunc("GET /ui/action/backup", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("GET /ui/backup/export", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("POST /ui/account/admin", h.withWebAuth(h.handleAccountAdmin))
	h.mux.HandleFunc("POST /ui/account/update_token", h.withWebAuth(h.handleAccountUpdateToken))
	h.mux.HandleFunc("GET /ui/inflight/data", h.withWebAuth(h.handleInFlightData))

	// 一键批量 / 单账号操作动作
	h.mux.HandleFunc("POST /ui/action/signin", h.withWebAuth(h.handleActionSignin))
	h.mux.HandleFunc("POST /ui/action/trial", h.withWebAuth(h.handleActionTrial))
	h.mux.HandleFunc("POST /ui/action/welfare", h.withWebAuth(h.handleActionWelfare))
	h.mux.HandleFunc("POST /ui/action/travel", h.withWebAuth(h.handleActionTravel))
	h.mux.HandleFunc("POST /ui/action/tasks", h.withWebAuth(h.handleActionTasks))
	h.mux.HandleFunc("POST /ui/action/bind_invite", h.withWebAuth(h.handleActionBindInvite))
	h.mux.HandleFunc("POST /ui/action/delete", h.withWebAuth(h.handleActionDelete))
	h.mux.HandleFunc("POST /ui/action/keepalive", h.withWebAuth(h.handleActionKeepalive))

	// 本地扫描与高级探测
	h.mux.HandleFunc("GET /ui/action/scan_local", h.withWebAuth(h.handleActionScanLocal))
	h.mux.HandleFunc("POST /ui/action/scan_local", h.withWebAuth(h.handleActionScanLocal))
	h.mux.HandleFunc("POST /ui/action/import_local", h.withWebAuth(h.handleActionImportLocal))
	h.mux.HandleFunc("GET /ui/action/credits_breakdown", h.withWebAuth(h.handleActionCreditsBreakdown))
	h.mux.HandleFunc("POST /ui/action/credits_breakdown", h.withWebAuth(h.handleActionCreditsBreakdown))
	h.mux.HandleFunc("POST /ui/action/probe_credits", h.withWebAuth(h.handleActionProbeCredits))
	h.mux.HandleFunc("GET /ui/action/campaigns", h.withWebAuth(h.handleActionCampaigns))

	// 配置与系统更新
	h.mux.HandleFunc("POST /ui/config/apikey", h.withWebAuth(h.handleConfigAPIKey))
	h.mux.HandleFunc("POST /ui/config/password", h.withWebAuth(h.handleConfigPassword))
	h.mux.HandleFunc("POST /ui/config/ip_whitelist", h.withWebAuth(h.handleConfigIPWhitelist))
	h.mux.HandleFunc("POST /ui/config/queue", h.withWebAuth(h.handleConfigQueue))
	h.mux.HandleFunc("GET /ui/system/update/check", h.withWebAuth(h.handleSystemUpdateCheck))
	h.mux.HandleFunc("POST /ui/system/update/do", h.withWebAuth(h.handleSystemUpdateDo))

	// 用户中心与多用户管理端点
	h.mux.HandleFunc("POST /ui/user/apikey/reset", h.withWebAuth(h.handleUserAPIKeyReset))
	h.mux.HandleFunc("GET /ui/admin/users", h.withWebAuth(h.handleAdminListUsers))
	h.mux.HandleFunc("POST /ui/admin/user/quota", h.withWebAuth(h.handleAdminSetUserQuota))
	h.mux.HandleFunc("POST /ui/admin/user/toggle", h.withWebAuth(h.handleAdminToggleUser))
	h.mux.HandleFunc("POST /ui/admin/user/role", h.withWebAuth(h.handleAdminSetUserRole))
	h.mux.HandleFunc("POST /ui/admin/user/delete", h.withWebAuth(h.handleAdminDeleteUser))
	h.mux.HandleFunc("POST /ui/admin/user/create", h.withWebAuth(h.handleAdminCreateUser))
	h.mux.HandleFunc("POST /ui/admin/user/reset_password", h.withWebAuth(h.handleAdminResetUserPassword))
	h.mux.HandleFunc("POST /ui/admin/user/reset_key", h.withWebAuth(h.handleAdminResetUserAPIKey))
	h.mux.HandleFunc("POST /ui/admin/system/allow_register", h.withWebAuth(h.handleAdminSetAllowRegister))

	// DeepSeek Harness (dsh) 进程管理端点
	h.mux.HandleFunc("GET /ui/dsh/status", h.withWebAuth(h.handleDshStatus))
	h.mux.HandleFunc("POST /ui/dsh/install", h.withWebAuth(h.handleDshInstall))
	h.mux.HandleFunc("POST /ui/dsh/reset", h.withWebAuth(h.handleDshReset))
	h.mux.HandleFunc("POST /ui/dsh/start", h.withWebAuth(h.handleDshStart))
	h.mux.HandleFunc("POST /ui/dsh/stop", h.withWebAuth(h.handleDshStop))
	h.mux.HandleFunc("POST /ui/dsh/export", h.withWebAuth(h.handleDshExport))
	h.mux.HandleFunc("GET /ui/dsh/export", h.withWebAuth(h.handleDshExport))
}

func (h *Handler) handlePortalHTML(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	_, _ = w.Write(portalHTML)
}

func (h *Handler) handleDashboardHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	_, _ = w.Write(dashboardHTML)
}

// WarmupCreditsAsync asynchronously syncs fresh credit balances across CN and Global accounts on startup.
func (h *Handler) WarmupCreditsAsync() {
	go h.syncPoolCreditsWithTimeout(15 * time.Second)
}

// handlePublicStatus returns sanitized unauthenticated gateway telemetry for the public landing portal.
func (h *Handler) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := 0, 0, 0, 0, 0
	if h.cfg.Pool != nil {
		total, healthy, _, _, _ = h.cfg.Pool.CountsDetailed()
	}
	modelsCount := len(h.modelList())

	cnServable := false
	globalServable := false
	if h.cfg.Pool != nil {
		cnServable = h.cfg.Pool.ServableForRealm("cn")
		globalServable = h.cfg.Pool.ServableForRealm("global")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service":      ServiceName,
		"version":      CurrentVersion,
		"status":       "online",
		"models_count": modelsCount,
		"dual_realm": map[string]bool{
			"cn":     cnServable,
			"global": globalServable,
		},
		"capacity": map[string]any{
			"ready":   healthy > 0,
			"healthy": healthy,
			"total":   total,
		},
	})
}

func (h *Handler) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	isFresh := r.URL.Query().Get("fresh") == "1" || r.URL.Query().Get("force") == "1"
	if isFresh {
		h.syncPoolCreditsWithTimeout(4 * time.Second)
		h.refreshLiveCatalogAsyncIfNeeded()
	}

	total, healthy, cooling, disabled, inFlightFull := 0, 0, 0, 0, 0
	if h.cfg.Pool != nil {
		total, healthy, cooling, disabled, inFlightFull = h.cfg.Pool.CountsDetailed()
	}
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}

	user := h.getWebSessionUser(r)

	authDir := h.getAuthDir()
	files, _ := auth.LoadAuthFiles(authDir)

	type AccountItem struct {
		UID               string                  `json:"uid"`
		Realm             string                  `json:"realm"`
		Nickname          string                  `json:"nickname"`
		Provider          string                  `json:"provider,omitempty"`
		Filename          string                  `json:"filename"`
		Credits           int64                   `json:"credits"`
		CreditsExpiring   int64                   `json:"credits_expiring,omitempty"`
		CreditDetail      *CreditBreakdown        `json:"credit_detail,omitempty"`
		Disabled          bool                    `json:"disabled"`
		DisabledReason    string                  `json:"disabled_reason"`
		ManualDisabled    bool                    `json:"manual_disabled"`
		ManualReason      string                  `json:"manual_reason"`
		Cooling           bool                    `json:"cooling"`
		CoolKind          string                  `json:"cool_kind,omitempty"`
		CoolRemaining     int64                   `json:"cool_remaining_sec,omitempty"`
		Reason            string                  `json:"reason,omitempty"`
		RateLimitedModels []pool.RateLimitedModel `json:"rate_limited_models,omitempty"`
		ModelCosts        []pool.ModelCostStatus  `json:"model_costs,omitempty"`
		Owner             string                  `json:"owner"`
		IsMyAccount       bool                    `json:"is_my_account"`
		InviteCode        string                  `json:"invite_code,omitempty"`
		InviteURL         string                  `json:"invite_url,omitempty"`
		CheckinStatus     string                  `json:"checkin_status,omitempty"`
		CheckinDetail     string                  `json:"checkin_detail,omitempty"`
		CheckinAt         string                  `json:"checkin_at,omitempty"`

		// Qoder-specific extras
		Domain       string `json:"domain,omitempty"`
		UserType     string `json:"user_type,omitempty"`
		Organization string `json:"organization,omitempty"`
		VPCEndpoint  string `json:"vpc_endpoint,omitempty"`
		HasPAT       bool   `json:"has_pat,omitempty"`
		HasDesktopFP bool   `json:"has_desktop_fp,omitempty"`
		InFlight     int    `json:"in_flight,omitempty"`
		SuccessCount int64  `json:"success_count,omitempty"`
		ErrTotal     int64  `json:"err_total,omitempty"`
		ExpiresAt    int64  `json:"expires_at,omitempty"`
	}

	var poolList []pool.Status
	if h.cfg.Pool != nil {
		poolList = h.cfg.Pool.List()
	}
	poolMap := make(map[string]pool.Status)
	for _, p := range poolList {
		poolMap[p.UID] = p
		poolMap[p.Realm+":"+p.UID] = p
		if p.FilePath != "" {
			poolMap[filepath.Base(p.FilePath)] = p
		}
	}

	seenKey := make(map[string]bool)
	seenAccount := make(map[string]bool)
	accounts := make([]AccountItem, 0, len(files)+len(poolList))

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
		if a.Provider() == "" {
			base := strings.ToLower(filepath.Base(f))
			if strings.Contains(base, "twitter") || strings.Contains(base, "-x-") {
				a.SetProvider("twitter")
			} else if strings.Contains(base, "google") {
				a.SetProvider("google")
			} else if strings.Contains(base, "github") {
				a.SetProvider("github")
			}
		}

		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		acctFingerprint := fmt.Sprintf("%s:%s:%s", a.Realm(), a.Provider(), strings.ToLower(a.RawUID()))
		if seenAccount[acctFingerprint] {
			continue
		}
		seenAccount[acctFingerprint] = true

		isMine := h.isAccountOwnedBy(a, user)
		key := fmt.Sprintf("%s:%s", a.Realm(), a.UID)
		rawKey := fmt.Sprintf("%s:%s", a.Realm(), a.RawUID())
		seenKey[key] = true
		seenKey[rawKey] = true
		seenKey[a.UID] = true
		seenKey[filepath.Base(f)] = true

		code := defaultInviteCode(a)
		item := AccountItem{
			UID:          a.UID,
			Realm:        a.Realm(),
			Nickname:     a.Nickname,
			Provider:     a.Provider(),
			Filename:     filepath.Base(f),
			Owner:        a.OwnerValue(),
			IsMyAccount:  isMine,
			InviteCode:   code,
			InviteURL:    defaultInviteURL(a, code),
			Domain:       a.DomainValue(),
			UserType:     a.UserTypeValue(),
			Organization: a.OrganizationName,
			VPCEndpoint:  a.VPCEndpointValue(),
			HasPAT:       a.PersonalTokenValue() != "",
			ExpiresAt:    a.ExpiresAt,
		}
		a.Lock()
		item.HasDesktopFP = a.CheckinFingerprint != nil && a.CheckinFingerprint.MachineToken != ""
		a.Unlock()

		var matched *pool.Status
		if p, ok := poolMap[filepath.Base(f)]; ok {
			matched = &p
		} else if p, ok := poolMap[a.UID]; ok {
			matched = &p
		} else if p, ok := poolMap[key]; ok {
			matched = &p
		} else if p, ok := poolMap[rawKey]; ok {
			matched = &p
		}

		if matched != nil {
			item.UID = matched.UID
			if matched.Provider != "" {
				item.Provider = matched.Provider
			}
			item.Credits = matched.Credits
			item.CreditsExpiring = matched.CreditsExpiring
			item.Disabled = matched.Disabled
			item.DisabledReason = matched.DisabledReason
			item.ManualDisabled = matched.ManualDisabled
			item.ManualReason = matched.ManualReason
			item.Cooling = matched.Cooling
			item.CoolKind = matched.CoolKind
			item.CoolRemaining = matched.CoolRemaining
			item.Reason = matched.Reason
			item.RateLimitedModels = matched.RateLimitedModels
			item.ModelCosts = matched.ModelCosts
			item.InFlight = matched.InFlight
			item.SuccessCount = matched.SuccessCount
			item.ErrTotal = matched.ErrTotal

			seenKey[matched.UID] = true
			seenKey[matched.Realm+":"+matched.UID] = true
			if matched.FilePath != "" {
				seenKey[filepath.Base(matched.FilePath)] = true
			}
			matchedFP := fmt.Sprintf("%s:%s:%s", matched.Realm, matched.Provider, strings.ToLower(auth.CleanRawUID(matched.UID)))
			seenAccount[matchedFP] = true
		}

		if a.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(a, item.UID, isFresh)
		}
		accounts = append(accounts, item)
	}

	// 并集补齐内存账号池账号
	for _, p := range poolList {
		poolKey := fmt.Sprintf("%s:%s", p.Realm, p.UID)
		if seenKey[p.UID] || seenKey[poolKey] {
			continue
		}
		authObj := h.cfg.Pool.AuthByUID(p.UID)
		if authObj != nil {
			rawKey := fmt.Sprintf("%s:%s", authObj.Realm(), authObj.RawUID())
			if seenKey[rawKey] || seenKey[authObj.RawUID()] {
				continue
			}
			fp := fmt.Sprintf("%s:%s:%s", authObj.Realm(), authObj.Provider(), strings.ToLower(authObj.RawUID()))
			if seenAccount[fp] {
				continue
			}
		} else {
			clean := auth.CleanRawUID(p.UID)
			fp := fmt.Sprintf("%s:%s:%s", p.Realm, p.Provider, strings.ToLower(clean))
			if seenAccount[fp] {
				continue
			}
		}
		if authObj != nil && !h.isAccountVisibleTo(authObj, user) {
			continue
		}

		owner := ""
		realm := p.Realm
		nickname := p.Nickname
		filename := fmt.Sprintf("qoder-%s.json", p.UID)
		domain, userType, orgName, vpc := "", "", "", ""
		hasPAT, hasFP := false, false
		var expAt int64

		if authObj != nil {
			owner = authObj.OwnerValue()
			realm = authObj.Realm()
			domain = authObj.DomainValue()
			userType = authObj.UserTypeValue()
			orgName = authObj.OrganizationName
			vpc = authObj.VPCEndpointValue()
			hasPAT = authObj.PersonalTokenValue() != ""
			expAt = authObj.ExpiresAt
			authObj.Lock()
			hasFP = authObj.CheckinFingerprint != nil && authObj.CheckinFingerprint.MachineToken != ""
			authObj.Unlock()

			if authObj.Nickname != "" {
				nickname = authObj.Nickname
			}
			if authObj.FilePath != "" {
				filename = filepath.Base(authObj.FilePath)
			}
		}

		isMine := false
		if authObj != nil {
			isMine = h.isAccountOwnedBy(authObj, user)
		} else if user == nil || user.Role == usermgr.RoleAdmin {
			isMine = true
		}

		seenKey[poolKey] = true
		seenKey[p.UID] = true
		prov := p.Provider
		if prov == "" && authObj != nil {
			prov = authObj.Provider()
		}
		poolInviteCode := ""
		if authObj != nil {
			poolInviteCode = defaultInviteCode(authObj)
		} else {
			poolInviteCode = auth.CleanRawUID(p.UID)
		}
		poolInviteURL := ""
		if authObj != nil {
			poolInviteURL = defaultInviteURL(authObj, poolInviteCode)
		} else if realm == "global" {
			poolInviteURL = "https://qoder.com/i/" + poolInviteCode
		} else {
			poolInviteURL = "https://qoder.com.cn/i/" + poolInviteCode
		}

		item := AccountItem{
			UID:               p.UID,
			Realm:             realm,
			Nickname:          nickname,
			Provider:          prov,
			Filename:          filename,
			Credits:           p.Credits,
			CreditsExpiring:   p.CreditsExpiring,
			Disabled:          p.Disabled,
			DisabledReason:    p.DisabledReason,
			ManualDisabled:    p.ManualDisabled,
			ManualReason:      p.ManualReason,
			Cooling:           p.Cooling,
			CoolKind:          p.CoolKind,
			CoolRemaining:     p.CoolRemaining,
			Reason:            p.Reason,
			RateLimitedModels: p.RateLimitedModels,
			ModelCosts:        p.ModelCosts,
			Owner:             owner,
			IsMyAccount:       isMine,
			InviteCode:        poolInviteCode,
			InviteURL:         poolInviteURL,
			Domain:            domain,
			UserType:          userType,
			Organization:      orgName,
			VPCEndpoint:       vpc,
			HasPAT:            hasPAT,
			HasDesktopFP:      hasFP,
			InFlight:          p.InFlight,
			SuccessCount:      p.SuccessCount,
			ErrTotal:          p.ErrTotal,
			ExpiresAt:         expAt,
		}
		if (item.Credits == 0 || getCachedCreditBreakdown(p.UID) == nil) && authObj != nil && authObj.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(authObj, p.UID, isFresh)
		}
		accounts = append(accounts, item)
	}

	// 最终去重与汇总
	dedupedAccounts := make([]AccountItem, 0, len(accounts))
	finalSeenUID := make(map[string]bool)
	finalSeenFP := make(map[string]bool)
	creditSummary := newCreditSummaryBuilder()
	todayCheckedCount, cnAccountCount := 0, 0

	for _, item := range accounts {
		fp := fmt.Sprintf("%s:%s:%s", item.Realm, item.Provider, strings.ToLower(auth.CleanRawUID(item.UID)))
		if finalSeenUID[item.UID] || (item.Provider != "" && finalSeenFP[fp]) {
			continue
		}
		finalSeenUID[item.UID] = true
		if item.Provider != "" {
			finalSeenFP[fp] = true
		}

		if bd := getCachedCreditBreakdown(item.UID); bd != nil {
			item.CreditDetail = bd
		} else {
			// 兜底构造单包结构，确保前端积分明细弹窗 100% 优雅展示
			item.CreditDetail = &CreditBreakdown{
				Remain:          float64(item.Credits),
				Total:           float64(item.Credits),
				ActiveTotal:     float64(item.Credits),
				BaseRemain:      float64(item.Credits),
				ExpiringRemain:  float64(item.CreditsExpiring),
				PackCount:       1,
				ActivePackCount: 1,
				Packs: []CreditPackDetail{
					{
						PackageName:    "Qoder 账户算力额度",
						Category:       "base",
						Remain:         float64(item.Credits),
						Total:          float64(item.Credits),
						IsActive:       item.Credits > 0,
						IsExpiringSoon: item.CreditsExpiring > 0,
					},
				},
			}
		}

		if h.cfg.Pool != nil {
			if rec, ok := h.cfg.Pool.GetCheckinResultToday(item.UID); ok {
				item.CheckinStatus = rec.Status
				item.CheckinDetail = rec.Detail
				item.CheckinAt = rec.UpdatedAt.In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
			}
		}

		if item.Realm == "cn" {
			cnAccountCount++
		}
		if item.CheckinStatus == "ok" || item.CheckinStatus == "already" {
			todayCheckedCount++
		}
		creditSummary.add(item.Realm, item.Disabled, item.ManualDisabled, item.Cooling, item.Credits, item.CreditsExpiring, item.CreditDetail, item.RateLimitedModels, item.ModelCosts)
		dedupedAccounts = append(dedupedAccounts, item)
	}
	accounts = dedupedAccounts

	statTotal, statHealthy, statCooling, statDisabled, statInFlight := total, healthy, cooling, disabled, inFlightFull
	if user != nil && user.Role != usermgr.RoleAdmin {
		statTotal = len(accounts)
		statHealthy, statCooling, statDisabled = 0, 0, 0
		for _, it := range accounts {
			if it.Disabled || it.ManualDisabled {
				statDisabled++
			} else if it.Cooling {
				statCooling++
			} else {
				statHealthy++
			}
		}
	}

	models := h.modelList()
	statsSnap := MetricsSnapshotOf()
	h.enrichCredits(&statsSnap)

	displayAPIKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		displayAPIKey = user.APIKey
	}

	dshSt := dshmgr.Status{}
	if h.cfg.DshMgr != nil {
		dshSt = h.cfg.DshMgr.GetStatus()
	}

	wm, mr, capD, defD, ft, bd := upstream.CurrentQueueSettings()

	localCands := cosy.ScanLocalCredentials()
	localReady := 0
	for _, c := range localCands {
		if c.Valid {
			localReady++
		}
	}

	creditSumBuilt := creditSummary.build()
	checkinSumBuilt := h.buildCheckinSummary(todayCheckedCount, cnAccountCount)
	netInfo := h.GetRuntimeNetworkInfo(r)

	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"status": map[string]any{
			"total":           statTotal,
			"healthy":         statHealthy,
			"cooling":         statCooling,
			"disabled":        statDisabled,
			"in_flight_full":  statInFlight,
			"sticky_sessions": sticky,
		},
		"accounts":        accounts,
		"creditSummary":   creditSumBuilt,
		"credit_summary":  creditSumBuilt,
		"checkinSummary":  checkinSumBuilt,
		"checkin_summary": checkinSumBuilt,
		"models":          models,
		"apiKey":          displayAPIKey,
		"api_key":         displayAPIKey,
		"ipWhitelist":     h.GetIPWhitelist(),
		"ip_whitelist":    h.GetIPWhitelist(),
		"networkInfo":     netInfo,
		"network_info":    netInfo,
		"hasPassword":     h.GetWebPassword() != "",
		"has_password":    h.GetWebPassword() != "",
		"currentVersion":  CurrentVersion,
		"current_version": CurrentVersion,
		"isDocker":        isDockerEnvironment(),
		"is_docker":       isDockerEnvironment(),
		"recentLogs":      GetRecentLogs(),
		"recent_logs":     GetRecentLogs(),
		"stats":           statsSnap,
		"metrics":         statsSnap,
		"currentUser":     user,
		"user":            user,
		"allowRegister":   h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),
		"allow_register":  h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),

		// Qoder extras
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"global_enabled": h.cfg.GlobalEnabled,
		"redis_mode":     h.cfg.RedisMode,
		"inflight":       h.inFlightTracker.Snapshot(),
		"inflight_count": h.inFlightTracker.Count(),
		"sticky_count":   sticky,
		"activities":     h.recentActivities(),
		"local_scan": map[string]any{
			"total": len(localCands),
			"valid": localReady,
		},
		"dsh": dshSt,
		"queue_config": map[string]any{
			"wait_mode":        wm,
			"max_retries":      mr,
			"wait_cap_sec":     int(capD.Seconds()),
			"wait_default_sec": int(defD.Seconds()),
			"wait_first_token": ft,
			"budget_sec":       int(bd.Seconds()),
		},
		"endpoints_health": func() any {
			if h.cfg.Upstream != nil {
				return h.cfg.Upstream.EndpointStatuses()
			}
			return nil
		}(),
		"ts":       time.Now().Unix(),
		"base_url": fmt.Sprintf("%s://%s", requestScheme(r), r.Host),
	})
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
}
