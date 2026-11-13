package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/upstream"
)

var dynamicModelsCache struct {
	sync.RWMutex
	lastFetch time.Time
	lastFail  time.Time
}

const (
	dynamicModelsTTL        = 5 * time.Minute
	modelsFetchFailCooldown = time.Minute
)

// models implements GET /v1/models aligned with workbuddy2api + Qoder capabilities.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	includeBare := r.URL.Query().Get("bare") == "true" ||
		r.URL.Query().Get("include_bare") == "true" ||
		r.Header.Get("X-Include-Bare-Models") == "true" ||
		strings.Contains(ua, "new-api") ||
		strings.Contains(ua, "one-api") ||
		strings.Contains(ua, "openai") ||
		strings.Contains(ua, "curl") ||
		strings.Contains(ua, "python") ||
		strings.Contains(ua, "langchain") ||
		r.URL.Query().Get("compat") == "bare"

	h.refreshLiveCatalogAsyncIfNeeded()

	var infos []upstream.ModelInfo
	if h != nil && h.cfg.Upstream != nil {
		infos = h.cfg.Upstream.MergedModelInfos(true, h.cfg.GlobalEnabled)
	}
	items := buildModelListEntries(infos, includeBare, h != nil && h.cfg.DshMgr != nil && h.cfg.DshMgr.GetStatus().Running, h)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func (h *Handler) refreshLiveCatalogAsyncIfNeeded() {
	if h == nil || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return
	}
	dynamicModelsCache.RLock()
	fresh := time.Since(dynamicModelsCache.lastFetch) < dynamicModelsTTL ||
		time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown
	dynamicModelsCache.RUnlock()
	if fresh {
		return
	}

	var accts []*auth.Auth
	for _, r := range []string{"global", "cn"} {
		if r == "global" && !h.cfg.GlobalEnabled {
			continue
		}
		if a := h.pickHealthyAccountForRealm(r); a != nil {
			accts = append(accts, a)
		}
	}
	if len(accts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	anyOK := false
	for _, a := range accts {
		if _, err := h.cfg.Upstream.FetchModels(ctx, a); err == nil {
			anyOK = true
		}
	}
	dynamicModelsCache.Lock()
	if anyOK {
		dynamicModelsCache.lastFetch = time.Now()
	} else {
		dynamicModelsCache.lastFail = time.Now()
	}
	dynamicModelsCache.Unlock()
}

func (h *Handler) pickHealthyAccountForRealm(realm string) *auth.Auth {
	if h == nil || h.cfg.Pool == nil {
		return nil
	}
	uids := h.cfg.Pool.AvailableUIDsForRealm(realm)
	for _, uid := range uids {
		a := h.cfg.Pool.AuthByUID(uid)
		if a != nil && a.AccessTokenValue() != "" {
			return a
		}
	}
	return nil
}

func buildModelListEntries(infos []upstream.ModelInfo, includeBare bool, dshRunning bool, h *Handler) []map[string]any {
	seen := make(map[string]bool)
	var out []map[string]any

	addItem := func(id string, mi upstream.ModelInfo) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		creditsLabel := "free"
		if mi.PriceFactor > 0 {
			creditsLabel = fmt.Sprintf("%.2fx", mi.PriceFactor)
		}
		entry := map[string]any{
			"id":              id,
			"key":             mi.Key,
			"display_name":    mi.DisplayName,
			"object":          "model",
			"created":         mi.Created,
			"owned_by":        "qoder",
			"realm":           mi.Realm,
			"realms":          mi.Realms,
			"supports_images": mi.SupportsImages,
			"is_reasoning":    mi.IsReasoning,
			"is_free":         mi.IsFree,
			"price_factor":    mi.PriceFactor,
			"credits":         creditsLabel,
			"context_length":  mi.ContextLength,
		}
		if len(mi.Efforts) > 0 {
			entry["reasoning_efforts"] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			entry["reasoning_default_effort"] = mi.DefaultEffort
		}
		out = append(out, entry)
	}

	for _, mi := range infos {
		if includeBare || len(mi.Realms) > 1 {
			addItem(mi.ID, mi)
			if mi.Key != "" && mi.Key != mi.ID {
				addItem(mi.Key, mi)
			}
		} else {
			for _, r := range mi.Realms {
				addItem(r+":"+mi.ID, mi)
			}
			addItem(mi.ID, mi)
		}
	}

	// Include configured virtual routes & auto-free alias.
	addVirtual := func(id string, desc string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, map[string]any{
			"id":             id,
			"object":         "model",
			"created":        time.Now().Unix(),
			"owned_by":       "qoder-gateway",
			"description":    desc,
			"context_length": 200000,
		})
	}
	addVirtual("auto-free", "Automatic 0-credit Qoder model failover pool (qfmodel/lmodel)")
	if h != nil {
		for k := range h.cfg.ModelRoutes {
			addVirtual(k, "Custom gateway virtual failover route")
		}
	}
	if dshRunning {
		addVirtual("dsh", "Embedded DeepSeek Web Harness proxy model")
	}
	return out
}

// isFreeTierModel determines whether an invocation targets a zero-credit model (guarding against accidental credit burn).
func (h *Handler) isFreeTierModel(realm, bareModel, reqModel string) bool {
	if IsFreeRouteAlias(reqModel) {
		return true
	}
	if upstream.IsFreeModel(bareModel, realm) {
		return true
	}
	if h != nil {
		for _, m := range h.cfg.FreeModels {
			_, fb := resolveModel(m)
			if strings.EqualFold(fb, bareModel) || strings.EqualFold(m, reqModel) {
				return true
			}
		}
	}
	return false
}

// catalogModelEntry looks up model capabilities (for image check & gateway_hint).
func (h *Handler) catalogModelEntry(bareModel, realm string) (upstream.ModelMeta, bool) {
	return upstream.CatalogMetaFor(bareModel, realm)
}
