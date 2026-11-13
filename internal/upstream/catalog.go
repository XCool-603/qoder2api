package upstream

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

//go:embed qoder_catalog_cn.json
var catalogCNRaw []byte

//go:embed qoder_catalog_intl.json
var catalogIntlRaw []byte

//go:embed baseprompt.json
var basePromptRaw []byte

// ThinkingEffortSpec describes one thinking effort level supported by a Qoder model.
type ThinkingEffortSpec struct {
	Description string `json:"description,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
}

// ThinkingModeSpec holds enabled/disabled thinking capabilities for a model.
type ThinkingModeSpec struct {
	Description string                        `json:"description,omitempty"`
	IsDefault   bool                          `json:"is_default,omitempty"`
	Efforts     map[string]ThinkingEffortSpec `json:"efforts,omitempty"`
}

// ThinkingConfig captures Qoder model thinking capabilities.
type ThinkingConfig struct {
	Enabled  *ThinkingModeSpec `json:"enabled,omitempty"`
	Disabled *ThinkingModeSpec `json:"disabled,omitempty"`
}

// ContextWindowSpec describes one context window option in Qoder's catalog.
type ContextWindowSpec struct {
	TokenCount int  `json:"token_count"`
	IsDefault  bool `json:"is_default,omitempty"`
}

// CreditDiscount describes time-boxed discount windows (e.g. off-peak multipliers).
type CreditDiscount struct {
	StartTime       string  `json:"start_time,omitempty"`
	EndTime         string  `json:"end_time,omitempty"`
	DiscountPercent float64 `json:"discount_percent,omitempty"`
	PriceFactor     float64 `json:"price_factor,omitempty"`
}

// ModelMeta matches Qoder's /algo/api/v2/model/list JSON item shape.
type ModelMeta struct {
	Key            string                       `json:"key"`
	Format         string                       `json:"format,omitempty"`
	Source         string                       `json:"source,omitempty"`
	Enable         bool                         `json:"enable"`
	DisplayName    string                       `json:"display_name"`
	IsVL           bool                         `json:"is_vl"`
	IsReasoning    bool                         `json:"is_reasoning"`
	IsDefault      bool                         `json:"is_default,omitempty"`
	IsFree         bool                         `json:"is_free,omitempty"`
	PriceFactor    float64                      `json:"price_factor"`
	MaxInputTokens int                          `json:"max_input_tokens"`
	MaxOutput      int                          `json:"max_output_tokens,omitempty"`
	ThinkingConfig *ThinkingConfig              `json:"thinking_config,omitempty"`
	ContextConfig  map[string]ContextWindowSpec `json:"context_config,omitempty"`
	CreditDiscount []CreditDiscount             `json:"credit_discount,omitempty"`
}

// ModelInfo exposes OpenAI-style + rich gateway metadata for /v1/models and console UI.
type ModelInfo struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"`
	DisplayName    string   `json:"display_name"`
	Object         string   `json:"object"`
	Created        int64    `json:"created"`
	OwnedBy        string   `json:"owned_by"`
	Realm          string   `json:"realm"`
	Realms         []string `json:"realms,omitempty"`
	SupportsImages bool     `json:"supports_images"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsFree         bool     `json:"is_free"`
	PriceFactor    float64  `json:"price_factor"`
	ContextLength  int      `json:"context_length"`
	Efforts        []string `json:"reasoning_efforts,omitempty"`
	DefaultEffort  string   `json:"reasoning_default_effort,omitempty"`
	Enable         bool     `json:"enable"`
}

var (
	staticCatalogOnce sync.Once
	staticCNModels    []ModelMeta
	staticIntlModels  []ModelMeta
	basePromptMap     map[string]any

	liveCatalogMu    sync.RWMutex
	liveCatalogCache = make(map[string]liveCatalogEntry)
	liveDisplayIndex = make(map[string]string)
)

type liveCatalogEntry struct {
	models    []ModelMeta
	fetchedAt time.Time
}

// Global-only & CN-only Qoder upstream model keys for deterministic auto-realm routing.
var (
	GlobalExclusiveModels = map[string]bool{
		"ultimate": true, "performance": true, "efficient": true,
		"smodel": true, "cmodel": true,
		"sonus": true, "cantus": true,
	}
	CNExclusiveModels = map[string]bool{
		"q37fmodel": true, "gm51model": true, "q36fmodel": true,
	}
)

// ModelAliases maps friendly client IDs / official display names to canonical Qoder keys.
var ModelAliases = map[string]string{
	// Routers
	"auto": "auto", "qoder-auto": "auto",
	"ultimate": "ultimate", "performance": "performance", "efficient": "efficient",
	// Claude / Sonus / Cantus (Global)
	"sonus": "smodel", "cantus": "cmodel",
	"claude-sonnet-4": "smodel", "claude-sonnet-4-5": "smodel", "claude-sonnet-4-6": "smodel", "claude-3-7-sonnet": "smodel",
	"claude-opus-4": "cmodel", "claude-opus-4-5": "cmodel", "claude-opus-4-6": "cmodel",
	// Qwen
	"qwen4-max": "qmodel_38max", "qwen4-max-preview": "qmodel_38max", "qwen4": "qmodel_38max",
	"qwen3.8-max": "qmodel_38max", "qwen3.8-max-preview": "qmodel_38max", "qmodel_preview": "qmodel_38max",
	"qwen3.8-flash": "qfmodel",
	"qwen3.7-max":   "qmodel_latest",
	"qwen3.7-plus":  "qmodel",
	"qwen3.7-flash": "q37fmodel",
	"qwen3.6-flash": "q36fmodel",
	// DeepSeek
	"deepseek-v4-pro": "dmodel", "deepseek-v4-flash": "dfmodel", "deepseek-flash": "dfmodel",
	// GLM
	"glm-5.3": "gmodel", "glm-5.3-flash": "gfmodel", "glm-5.2": "gm51model",
	// Kimi
	"kimi-k3": "kmodel_latest", "kimi-k3.1-pro": "kmodel_latest",
	"kimi-k2.8-preview": "kmodel", "kimi-k2.7": "kmodel", "kimi-k2.7-code": "kmodel",
	// MiniMax
	"minimax-m3": "mmodel", "minimax-m2.7": "mmodel", "minimax-m2.5": "mmodel",
	// QoDuck
	"qoduck": "lmodel",
}

// EffortRank defines ordered intensity levels accepted by Qoder thinking models.
var EffortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

var effortOffValues = map[string]bool{
	"none": true, "off": true, "disabled": true, "disable": true, "false": true, "0": true, "no": true,
}

func initStaticCatalog() {
	staticCatalogOnce.Do(func() {
		_ = json.Unmarshal(catalogCNRaw, &staticCNModels)
		_ = json.Unmarshal(catalogIntlRaw, &staticIntlModels)
		_ = json.Unmarshal(basePromptRaw, &basePromptMap)
	})
}

// EmbeddedTemplate returns a deep copy of the stripped official Qoder request envelope.
func EmbeddedTemplate() map[string]any {
	initStaticCatalog()
	if basePromptMap == nil {
		return map[string]any{}
	}
	b, _ := json.Marshal(basePromptMap)
	var cp map[string]any
	_ = json.Unmarshal(b, &cp)
	return cp
}

// StaticModelsForRealm returns a copy of the embedded static catalog for CN or Global.
func StaticModelsForRealm(realm string) []ModelMeta {
	initStaticCatalog()
	src := staticIntlModels
	if NormalizeRealm(realm) == RealmCN {
		src = staticCNModels
	}
	out := make([]ModelMeta, len(src))
	copy(out, src)
	return out
}

// SplitRealmPrefix parses prefixes like "cn:auto", "global/smodel", or "gl/kmodel".
func SplitRealmPrefix(model string) (explicitRealm, bareModel string) {
	m := strings.TrimSpace(model)
	low := strings.ToLower(m)
	for _, p := range []struct {
		prefix string
		realm  string
	}{
		{"cn:", RealmCN}, {"cn/", RealmCN},
		{"global:", RealmGlobal}, {"global/", RealmGlobal},
		{"intl:", RealmGlobal}, {"intl/", RealmGlobal},
		{"gl:", RealmGlobal}, {"gl/", RealmGlobal},
	} {
		if strings.HasPrefix(low, p.prefix) {
			return p.realm, strings.TrimSpace(m[len(p.prefix):])
		}
	}
	return "", m
}

// ResolveUpstreamKey maps a client model ID or display name to its Qoder model_key.
func ResolveUpstreamKey(model, realm string) string {
	_, bare := SplitRealmPrefix(model)
	low := strings.ToLower(strings.TrimSpace(bare))
	if low == "" {
		return "auto"
	}
	if k, ok := ModelAliases[low]; ok {
		return k
	}
	// Check live catalog first, then static catalog for target realm and both realms.
	for _, m := range CatalogModelsForRealm(realm) {
		if strings.EqualFold(m.Key, low) || strings.EqualFold(m.DisplayName, low) {
			return m.Key
		}
	}
	liveCatalogMu.RLock()
	if k, ok := liveDisplayIndex[low]; ok {
		liveCatalogMu.RUnlock()
		return k
	}
	liveCatalogMu.RUnlock()

	for _, r := range []string{RealmCN, RealmGlobal} {
		for _, m := range StaticModelsForRealm(r) {
			if strings.EqualFold(m.Key, low) || strings.EqualFold(m.DisplayName, low) {
				return m.Key
			}
		}
	}
	return bare
}

// AutoRealmForModel inspects model exclusivity or prefixes to select CN vs Global automatically.
func AutoRealmForModel(model string) (realm string, reason string) {
	if exp, _ := SplitRealmPrefix(model); exp != "" {
		return exp, "explicit prefix"
	}
	key := ResolveUpstreamKey(model, "")
	lowKey := strings.ToLower(key)
	_, bare := SplitRealmPrefix(model)
	lowBare := strings.ToLower(bare)

	if GlobalExclusiveModels[lowKey] || GlobalExclusiveModels[lowBare] {
		return RealmGlobal, "global-exclusive model " + key
	}
	if CNExclusiveModels[lowKey] || CNExclusiveModels[lowBare] {
		return RealmCN, "cn-exclusive model " + key
	}
	return "", ""
}

// ExclusiveRealm returns "cn" or "global" when a model strictly exists in one Qoder deployment.
func ExclusiveRealm(model string) string {
	key := strings.ToLower(ResolveUpstreamKey(model, ""))
	if GlobalExclusiveModels[key] {
		return RealmGlobal
	}
	if CNExclusiveModels[key] {
		return RealmCN
	}
	return ""
}

// CatalogMetaFor looks up rich ModelMeta (live > local QMC > static) for a model key.
func CatalogMetaFor(modelKey, realm string) (ModelMeta, bool) {
	key := ResolveUpstreamKey(modelKey, realm)
	for _, m := range CatalogModelsForRealm(realm) {
		if strings.EqualFold(m.Key, key) {
			return m, true
		}
	}
	for _, r := range []string{RealmGlobal, RealmCN} {
		for _, m := range StaticModelsForRealm(r) {
			if strings.EqualFold(m.Key, key) {
				return m, true
			}
		}
	}
	return ModelMeta{Key: key, DisplayName: key, MaxInputTokens: 180000}, false
}

// CatalogModelsForRealm returns cached live models if fresh (5m), else local QMC cache or static catalog.
func CatalogModelsForRealm(realm string) []ModelMeta {
	r := NormalizeRealm(realm)
	liveCatalogMu.RLock()
	entry, ok := liveCatalogCache[r]
	liveCatalogMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < 5*time.Minute && len(entry.models) > 0 {
		out := make([]ModelMeta, len(entry.models))
		copy(out, entry.models)
		return out
	}
	if qmc := loadLocalQMCModels(r); len(qmc) > 0 {
		return qmc
	}
	return StaticModelsForRealm(r)
}

func loadLocalQMCModels(realm string) []ModelMeta {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	sub := ".qoder"
	if NormalizeRealm(realm) == RealmCN {
		sub = ".qoder-cn"
	}
	modelsDir := filepath.Join(home, sub, ".models")
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		uid := ent.Name()
		for _, fname := range []string{"catalog-v7", "catalog-v6"} {
			blob, err := os.ReadFile(filepath.Join(modelsDir, uid, fname))
			if err != nil {
				continue
			}
			plain, err := cosy.DecryptQMCCatalog(string(blob), uid)
			if err != nil || len(plain) == 0 {
				continue
			}
			var arr []ModelMeta
			if json.Unmarshal(plain, &arr) == nil && len(arr) > 0 {
				return arr
			}
			var wrapped struct {
				Models []ModelMeta `json:"models"`
				Data   []ModelMeta `json:"data"`
			}
			if json.Unmarshal(plain, &wrapped) == nil {
				if len(wrapped.Models) > 0 {
					return wrapped.Models
				}
				if len(wrapped.Data) > 0 {
					return wrapped.Data
				}
			}
		}
	}
	return nil
}

// IsFreeModel reports whether a model is genuinely zero-credit (price_factor == 0).
func IsFreeModel(model, realm string) bool {
	if m, ok := CatalogMetaFor(model, realm); ok {
		return m.PriceFactor == 0
	}
	key := strings.ToLower(ResolveUpstreamKey(model, realm))
	return key == "qfmodel" || key == "lmodel"
}

// DefaultTestModel picks the cheapest 0-credit model in realm for health checks, falling back to auto.
func DefaultTestModel(realm string) string {
	models := CatalogModelsForRealm(realm)
	for _, m := range models {
		if m.Enable && m.PriceFactor == 0 && m.Key != "" {
			return m.Key
		}
	}
	for _, m := range models {
		if m.Enable && m.IsFree && m.Key != "" {
			return m.Key
		}
	}
	return "auto"
}

// IsDeepSeekModel reports whether model targets Qoder's DeepSeek family (needs reasoning_content history).
func IsDeepSeekModel(model, modelKey string) bool {
	for _, s := range []string{modelKey, model} {
		low := strings.ToLower(strings.TrimSpace(s))
		if low == "dmodel" || low == "dfmodel" || strings.Contains(low, "deepseek") {
			return true
		}
	}
	return false
}

// IsKimiModel reports whether model targets Qoder's Kimi family (needs strict tool protocol prompt).
func IsKimiModel(model, modelKey string) bool {
	for _, s := range []string{modelKey, model} {
		low := strings.ToLower(strings.TrimSpace(s))
		if low == "kmodel" || low == "kmodel_latest" || strings.Contains(low, "kimi") {
			return true
		}
	}
	return false
}

// SupportedEfforts extracts ordered valid reasoning_effort values from ThinkingConfig.
func SupportedEfforts(meta ModelMeta) []string {
	if meta.ThinkingConfig == nil || meta.ThinkingConfig.Enabled == nil || len(meta.ThinkingConfig.Enabled.Efforts) == 0 {
		return nil
	}
	order := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	var out []string
	for _, e := range order {
		if _, ok := meta.ThinkingConfig.Enabled.Efforts[e]; ok {
			out = append(out, e)
		}
	}
	return out
}

// DefaultEffort returns the catalog default thinking effort when specified.
func DefaultEffort(meta ModelMeta) string {
	if meta.ThinkingConfig == nil || meta.ThinkingConfig.Enabled == nil {
		return ""
	}
	for name, spec := range meta.ThinkingConfig.Enabled.Efforts {
		if spec.IsDefault {
			return name
		}
	}
	sup := SupportedEfforts(meta)
	if len(sup) > 0 {
		return sup[len(sup)/2]
	}
	return ""
}

// NormalizeReasoningEffort maps client reasoning_effort values onto the model's supported values.
func NormalizeReasoningEffort(effort string, meta ModelMeta) (normalized string, note string) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return "", ""
	}
	if effortOffValues[e] {
		return "none", ""
	}
	if meta.ThinkingConfig == nil {
		return e, ""
	}
	sup := SupportedEfforts(meta)
	if len(sup) == 0 {
		return "", "dropped (model only supports on/none toggle)"
	}
	for _, s := range sup {
		if s == e {
			return e, ""
		}
	}
	want, ok := EffortRank[e]
	if !ok {
		def := DefaultEffort(meta)
		if def == "" {
			def = sup[0]
		}
		return def, fmt.Sprintf("unknown level %s -> %s", e, def)
	}
	def := DefaultEffort(meta)
	defRank := EffortRank[def]
	best := sup[0]
	bestD1 := absInt(EffortRank[best] - want)
	bestD2 := absInt(EffortRank[best] - defRank)
	for _, cand := range sup[1:] {
		d1 := absInt(EffortRank[cand] - want)
		d2 := absInt(EffortRank[cand] - defRank)
		if d1 < bestD1 || (d1 == bestD1 && d2 < bestD2) {
			best, bestD1, bestD2 = cand, d1, d2
		}
	}
	return best, fmt.Sprintf("unsupported level %s -> %s", e, best)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// MaxContextLength returns the effective maximum input token budget for a catalog entry.
func MaxContextLength(meta ModelMeta) int {
	maxT := meta.MaxInputTokens
	for _, cw := range meta.ContextConfig {
		if cw.TokenCount > maxT {
			maxT = cw.TokenCount
		}
	}
	if maxT <= 0 {
		return 180000
	}
	return maxT
}

func authToCosyIdentity(a *auth.Auth) cosy.AuthIdentity {
	if a == nil {
		return cosy.AuthIdentity{}
	}
	return cosy.AuthIdentity{
		Name:               a.Nickname,
		Aid:                a.UID,
		Uid:                a.UID,
		OrganizationId:     a.OrganizationID,
		OrganizationName:   a.OrganizationName,
		UserType:           a.UserTypeValue(),
		SecurityOauthToken: a.AccessTokenValue(),
		RefreshToken:       a.RefreshTokenValue(),
	}
}

// FetchModels fetches live models from Qoder (/algo/api/v2/model/list?Encode=1) merging chat+app scenes.
func (c *Client) FetchModels(ctx context.Context, a *auth.Auth) ([]ModelMeta, error) {
	if a == nil {
		return nil, fmt.Errorf("missing auth for FetchModels")
	}
	realm := NormalizeRealm(a.Realm())
	sess, err := c.sessionCache().Get(authToCosyIdentity(a))
	if err != nil {
		return nil, err
	}

	gateways := c.OrderedGateways(a)
	var chatModels, appModels []ModelMeta
	var lastErr error

	fetchScene := func(scene string) ([]ModelMeta, error) {
		pathWithQuery := PathModelList + "&scene=" + scene
		pathOnly := "/api/v2/model/list" // PathWithoutAlgo = strip "/algo" + drop query
		bodyStr := "{}"
		encodedBody, err := cosy.Encode([]byte(bodyStr))
		if err != nil {
			return nil, err
		}
		for _, gw := range gateways {
			fullURL := strings.TrimRight(gw, "/") + pathWithQuery
			req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
			if rerr != nil {
				lastErr = rerr
				continue
			}
			c.applyInferenceHeaders(req, sess, encodedBody, pathOnly, "", false, a)
			resp, derr := c.httpClient().Do(req)
			if derr != nil {
				c.Endpoints.noteFailure("model-"+realm, gw, derr.Error())
				lastErr = derr
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				ue := ClassifyError(resp.StatusCode, string(raw))
				c.Endpoints.noteFailure("model-"+realm, gw, ue.Msg)
				lastErr = ue
				if ue.Kind == ErrSessionDead {
					return nil, ue
				}
				continue
			}
			c.Endpoints.noteSuccess("model-"+realm, gw)
			decoded, _ := cosy.Decode(strings.TrimSpace(string(raw)))
			if len(decoded) == 0 {
				decoded = raw
			}
			var arr []ModelMeta
			if json.Unmarshal(decoded, &arr) == nil && len(arr) > 0 {
				return arr, nil
			}
			var envelope struct {
				Data   []ModelMeta `json:"data"`
				Models []ModelMeta `json:"models"`
			}
			if json.Unmarshal(decoded, &envelope) == nil {
				if len(envelope.Data) > 0 {
					return envelope.Data, nil
				}
				if len(envelope.Models) > 0 {
					return envelope.Models, nil
				}
			}
			lastErr = fmt.Errorf("unexpected model list payload: %s", string(raw[:minInt(len(raw), 160)]))
		}
		return nil, lastErr
	}

	chatModels, cerr := fetchScene("chat")
	appModels, aerr := fetchScene("app")
	if len(chatModels) == 0 && len(appModels) == 0 {
		if cerr != nil {
			return nil, cerr
		}
		return nil, aerr
	}

	// Merge chat + app, letting chat win on duplicates.
	mergedMap := make(map[string]ModelMeta)
	var order []string
	for _, m := range appModels {
		if m.Key == "" {
			continue
		}
		if _, exists := mergedMap[m.Key]; !exists {
			order = append(order, m.Key)
		}
		mergedMap[m.Key] = m
	}
	for _, m := range chatModels {
		if m.Key == "" {
			continue
		}
		if _, exists := mergedMap[m.Key]; !exists {
			order = append(order, m.Key)
		}
		mergedMap[m.Key] = m
	}
	merged := make([]ModelMeta, 0, len(order))
	for _, k := range order {
		merged = append(merged, mergedMap[k])
	}

	liveCatalogMu.Lock()
	liveCatalogCache[realm] = liveCatalogEntry{models: merged, fetchedAt: time.Now()}
	for _, m := range merged {
		if m.DisplayName != "" && m.Key != "" {
			liveDisplayIndex[strings.ToLower(strings.TrimSpace(m.DisplayName))] = m.Key
		}
	}
	liveCatalogMu.Unlock()
	return merged, nil
}

// MergedModelInfos builds the unified /v1/models list across enabled realms.
func (c *Client) MergedModelInfos(includeCN, includeGlobal bool) []ModelInfo {
	byID := make(map[string]*ModelInfo)
	var order []string
	now := time.Now().Unix()

	addRealm := func(realm string) {
		for _, m := range CatalogModelsForRealm(realm) {
			if m.Key == "" {
				continue
			}
			id := m.DisplayName
			if id == "" {
				id = m.Key
			}
			efforts := SupportedEfforts(m)
			defEff := DefaultEffort(m)
			if existing, ok := byID[id]; ok {
				existing.Realms = appendUniqueStr(existing.Realms, realm)
				if !existing.SupportsImages && m.IsVL {
					existing.SupportsImages = true
				}
				if !existing.IsReasoning && m.IsReasoning {
					existing.IsReasoning = true
				}
				continue
			}
			info := &ModelInfo{
				ID:             id,
				Key:            m.Key,
				DisplayName:    m.DisplayName,
				Object:         "model",
				Created:        now,
				OwnedBy:        "qoder",
				Realm:          realm,
				Realms:         []string{realm},
				SupportsImages: m.IsVL,
				IsReasoning:    m.IsReasoning,
				IsFree:         m.PriceFactor == 0,
				PriceFactor:    m.PriceFactor,
				ContextLength:  MaxContextLength(m),
				Efforts:        efforts,
				DefaultEffort:  defEff,
				Enable:         m.Enable,
			}
			byID[id] = info
			order = append(order, id)
		}
	}
	if includeGlobal {
		addRealm(RealmGlobal)
	}
	if includeCN {
		addRealm(RealmCN)
	}
	out := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func appendUniqueStr(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
