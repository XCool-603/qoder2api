package server

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ModelCircuitBreaker tracks consecutive per-model failures and triggers transient cooling.
type ModelCircuitBreaker struct {
	mu       sync.RWMutex
	failures map[string]int
	cooling  map[string]time.Time
}

// NewModelCircuitBreaker creates a ModelCircuitBreaker.
func NewModelCircuitBreaker() *ModelCircuitBreaker {
	return &ModelCircuitBreaker{
		failures: make(map[string]int),
		cooling:  make(map[string]time.Time),
	}
}

// IsCooling reports whether model is currently inside circuit-breaker cooldown.
func (b *ModelCircuitBreaker) IsCooling(model string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	until, ok := b.cooling[model]
	return ok && time.Now().Before(until)
}

// NoteFailure increments failures and activates cooldown at threshold (>=2).
func (b *ModelCircuitBreaker) NoteFailure(model string, cooldown time.Duration) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[model]++
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	if b.failures[model] >= 2 {
		b.cooling[model] = time.Now().Add(cooldown)
		log.Printf("WARN: [circuit-breaker] model %s reached %d failures, cooling %v", model, b.failures[model], cooldown)
	}
}

// Trip opens circuit breaker immediately for a model (e.g. deterministic 115/model unavailable).
func (b *ModelCircuitBreaker) Trip(model string, cooldown time.Duration) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[model] = 3
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	b.cooling[model] = time.Now().Add(cooldown)
	log.Printf("WARN: [circuit-breaker] model %s tripped for %v", model, cooldown)
}

// NoteSuccess resets failure counters and clears cooldown on success.
func (b *ModelCircuitBreaker) NoteSuccess(model string) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, model)
	delete(b.cooling, model)
}

// IsFreeCredits parses credit multiplier text ("0", "0.0x", "free") and reports whether cost is zero.
func IsFreeCredits(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSuffix(s, "credit")
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, "x")
	s = strings.TrimSpace(s)
	switch s {
	case "0", "0.0", "0.00", "free", "免费":
		return true
	}
	if val, err := strconv.ParseFloat(s, 64); err == nil {
		return val <= 0.00001
	}
	return false
}

// IsFreeRouteAlias reports whether model matches built-in zero-credit routing aliases.
func IsFreeRouteAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto-free", "free", "free-models", "free-pool", "auto_free", "free_model", "free-tier", "免费", "免费模型":
		return true
	}
	return false
}

// GetFreeModelCandidates builds ordered Qoder 0-credit model candidates prioritizing healthy accounts.
func (h *Handler) GetFreeModelCandidates() []string {
	var poolList []string
	if h != nil && len(h.cfg.FreeModels) > 0 {
		poolList = append(poolList, h.cfg.FreeModels...)
	}
	if h != nil && h.cfg.ModelRoutes != nil {
		for _, alias := range []string{"auto-free", "free"} {
			if configured, ok := h.cfg.ModelRoutes[alias]; ok && len(configured) > 0 {
				poolList = append(poolList, configured...)
			}
		}
	}
	if h != nil && h.cfg.Upstream != nil {
		for _, mi := range h.cfg.Upstream.MergedModelInfos(true, h.cfg.GlobalEnabled) {
			if mi.IsFree {
				for _, r := range mi.Realms {
					poolList = append(poolList, r+":"+mi.Key)
				}
				poolList = append(poolList, mi.Key)
			}
		}
	}

	builtinDefaults := []string{
		"cn:qfmodel",
		"global:qfmodel",
		"cn:lmodel",
		"global:lmodel",
		"qfmodel",
		"lmodel",
	}
	poolList = append(poolList, builtinDefaults...)

	seen := make(map[string]bool)
	var deduped []string
	for _, m := range poolList {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		deduped = append(deduped, m)
	}
	res := h.SortCandidatesByHealth(deduped)
	if len(res) == 0 {
		return builtinDefaults
	}
	return res
}

// SortCandidatesByHealth orders candidates: healthy accounts > idle backup > circuit-broken models.
func (h *Handler) SortCandidatesByHealth(candidates []string) []string {
	var healthy, backup, cooling []string
	seen := make(map[string]bool)
	for _, m := range candidates {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if h != nil && h.modelBreaker != nil && h.modelBreaker.IsCooling(m) {
			cooling = append(cooling, m)
			continue
		}
		realm, bare := h.resolveModelForHandler(m)
		hasAcct := false
		if h != nil && h.cfg.Pool != nil {
			if len(h.cfg.Pool.AvailableUIDsForModelRealm(bare, realm)) > 0 {
				hasAcct = true
			}
		}
		if hasAcct {
			healthy = append(healthy, m)
		} else {
			backup = append(backup, m)
		}
	}
	out := make([]string, 0, len(seen))
	out = append(out, healthy...)
	out = append(out, backup...)
	out = append(out, cooling...)
	return out
}

func canCandidateCrossFailover(candModel, origModel, realm, bareModel string, totalInRealm int) bool {
	if excl := upstreamExclusive(bareModel); excl != "" && excl != realm {
		return false
	}
	hasExplicit := strings.HasPrefix(origModel, "cn:") || strings.HasPrefix(origModel, "global:") ||
		strings.HasPrefix(candModel, "cn:") || strings.HasPrefix(candModel, "global:")
	return !hasExplicit || totalInRealm > 0
}

func upstreamExclusive(bare string) string {
	// Import cycle-safe helper: global-exclusive models never cross-failover to CN.
	switch strings.ToLower(strings.TrimSpace(bare)) {
	case "ultimate", "performance", "efficient", "smodel", "cmodel", "sonus", "cantus":
		return "global"
	case "q37fmodel", "gm51model", "q36fmodel":
		return "cn"
	default:
		return ""
	}
}

func (h *Handler) hasHealthyForCandidate(candModel, origModel, owner string) bool {
	if h == nil || h.cfg.Pool == nil {
		return false
	}
	realm, bare := h.resolveModelForHandler(candModel)
	if h.cfg.Pool.HasHealthyForModelRealmAndOwner(bare, realm, owner) {
		return true
	}
	totalInRealm, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
	if canCandidateCrossFailover(candModel, origModel, realm, bare, totalInRealm) {
		other := "global"
		if realm == "global" {
			other = "cn"
		}
		if other == "global" && !h.cfg.GlobalEnabled {
			return false
		}
		if h.cfg.Pool.HasHealthyForModelRealmAndOwner(bare, other, owner) {
			return true
		}
	}
	return false
}

func (h *Handler) hasHealthyCandidateAfter(candidates []string, afterIdx int, origModel, owner string) bool {
	if h == nil || h.cfg.Pool == nil {
		return false
	}
	for j := afterIdx + 1; j < len(candidates); j++ {
		m := candidates[j]
		if h.modelBreaker != nil && h.modelBreaker.IsCooling(m) {
			continue
		}
		if h.hasHealthyForCandidate(m, origModel, owner) {
			return true
		}
	}
	return false
}

// resolveModelCandidates returns failover ordered target candidates for virtual routes/free aliases.
func (h *Handler) resolveModelCandidates(reqModel string) []string {
	if IsFreeRouteAlias(reqModel) {
		return h.GetFreeModelCandidates()
	}
	lower := strings.ToLower(strings.TrimSpace(reqModel))
	if h != nil && h.cfg.ModelRoutes != nil {
		if configured, ok := h.cfg.ModelRoutes[reqModel]; ok && len(configured) > 0 {
			return h.SortCandidatesByHealth(configured)
		}
		if configured, ok := h.cfg.ModelRoutes[lower]; ok && len(configured) > 0 {
			return h.SortCandidatesByHealth(configured)
		}
	}
	return []string{reqModel}
}
