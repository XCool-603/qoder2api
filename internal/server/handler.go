package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/dshmgr"
	"qoder2api/internal/pool"
	"qoder2api/internal/scheduler"
	"qoder2api/internal/session"
	"qoder2api/internal/upstream"
	"qoder2api/internal/usermgr"
)

type contextKey string

const userContextKey contextKey = "qd2a_user"

// ServiceName and CurrentVersion identify this gateway in runtime logs and update checks.
const (
	ServiceName    = "qoder2api"
	CurrentVersion = "v1.0.0"
)

//go:embed portal.html
var portalHTML []byte

//go:embed dashboard.html
var dashboardHTML []byte

// Config captures dependencies and policy configuration for the gateway HTTP handler.
type Config struct {
	Pool               *pool.Pool
	Upstream           *upstream.Client
	Scheduler          *scheduler.Scheduler
	Sessions           *session.Router
	UserMgr            *usermgr.Manager
	DshMgr             *dshmgr.Manager
	APIKey             string
	WebPassword        string
	Version            string
	RefreshSkew        time.Duration
	SoftCooldown       time.Duration
	MaxRotate          int
	PromptMode         string
	SystemPrompt       string
	GlobalEnabled      bool
	AdminEnabled       bool
	RedisMode          string
	ModelRoutes        map[string][]string
	ModelAliases       map[string]string
	FreeModels         []string
	ListenAddr         string
	StateFile          string
	AuthDir            string
	ConfigPath         string
	IPWhitelistEnabled bool
	AllowedIPs         []string
	AllowLocalhost     *bool
	AllowRemoteAdmin   bool
	AlertMgr           *AlertManager
	CheckinEnabled     *bool
	CheckinHours       []int

	PersistSettingFn func(key, value string)
	PersistAnyFn     func(key string, value any)
	StickyCount      func() int
	StopFunc         func()
}

// Handler serves OpenAI, Anthropic, Codex Responses, Management APIs, Public Portal, and Admin Console.
type Handler struct {
	cfg      Config
	mux      *http.ServeMux
	sched    *scheduler.Scheduler
	alertMgr *AlertManager
	wafIP    *wafIPGate

	modelBreaker    *ModelCircuitBreaker
	inFlightTracker *InFlightTracker

	dynMu sync.RWMutex

	ipMu       sync.RWMutex
	ipWL       IPWhitelistConfig
	ipPrefixes []netip.Prefix
	runtimeIPs []string

	oauthMu       sync.Mutex
	oauthSessions map[string]*pendingOAuthSession

	activityMu sync.RWMutex
	activities []ActivityRecord
}

// ActivityRecord captures Qoder check-in and Pro benefit actions shown in the dashboard.
type ActivityRecord struct {
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	OK       bool   `json:"ok"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm"`
	Detail   string `json:"detail"`
	Amount   int    `json:"amount"`
}

// NewHandler constructs a Handler matching test and embedding expectations.
func NewHandler(cfg Config) *Handler {
	h, _ := New(cfg)
	return h
}

// New constructs and wires a ready-to-serve Handler.
func New(cfg Config) (*Handler, error) {
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 3 * time.Minute
	}
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 8
	}
	alertMgr := cfg.AlertMgr
	if alertMgr == nil {
		alertMgr = NewAlertManager(AlertConfig{})
	}
	h := &Handler{
		cfg:             cfg,
		mux:             http.NewServeMux(),
		sched:           cfg.Scheduler,
		alertMgr:        alertMgr,
		wafIP:           new(wafIPGate),
		modelBreaker:    NewModelCircuitBreaker(),
		inFlightTracker: NewInFlightTracker(),
		oauthSessions:   make(map[string]*pendingOAuthSession),
	}
	h.initIPWhitelist(cfg)
	if cfg.Upstream != nil {
		cfg.Upstream.SetActivityHook(func(kind string, ok bool, uid, nickname, realm, detail string, amount int) {
			h.recordActivity(kind, ok, uid, nickname, realm, detail, amount)
		})
	}
	h.registerRoutes()
	return h, nil
}

func (h *Handler) recordActivity(kind string, ok bool, uid, nickname, realm, detail string, amount int) {
	if h == nil {
		return
	}
	shortUID := uid
	if len(shortUID) > 8 {
		shortUID = shortUID[:8]
	}
	rec := ActivityRecord{
		TS:       time.Now().Unix(),
		Kind:     kind,
		OK:       ok,
		UID:      shortUID,
		Nickname: nickname,
		Realm:    realm,
		Detail:   detail,
		Amount:   amount,
	}
	h.activityMu.Lock()
	h.activities = append([]ActivityRecord{rec}, h.activities...)
	if len(h.activities) > 100 {
		h.activities = h.activities[:100]
	}
	h.activityMu.Unlock()
}

func (h *Handler) recentActivities() []ActivityRecord {
	if h == nil {
		return nil
	}
	h.activityMu.RLock()
	defer h.activityMu.RUnlock()
	out := make([]ActivityRecord, len(h.activities))
	copy(out, h.activities)
	return out
}

// SetAPIKey updates the legacy admin/master API key at runtime.
func (h *Handler) SetAPIKey(key string) {
	h.dynMu.Lock()
	h.cfg.APIKey = strings.TrimSpace(key)
	h.dynMu.Unlock()
}

// GetAPIKey returns the active legacy admin/master API key.
func (h *Handler) GetAPIKey() string {
	h.dynMu.RLock()
	defer h.dynMu.RUnlock()
	return h.cfg.APIKey
}

// SetWebPassword updates the dashboard admin password at runtime.
func (h *Handler) SetWebPassword(pw string) {
	h.dynMu.Lock()
	h.cfg.WebPassword = strings.TrimSpace(pw)
	h.dynMu.Unlock()
}

// GetWebPassword reads the active dashboard password.
func (h *Handler) GetWebPassword() string {
	h.dynMu.RLock()
	defer h.dynMu.RUnlock()
	return h.cfg.WebPassword
}

func (h *Handler) savePersistentSetting(key, value string) {
	if h == nil {
		return
	}
	if h.cfg.PersistSettingFn != nil {
		h.cfg.PersistSettingFn(key, value)
		return
	}
	h.writeSettingsFileKey(key, value)
}

func (h *Handler) savePersistentAny(key string, value any) {
	if h == nil {
		return
	}
	if h.cfg.PersistAnyFn != nil {
		h.cfg.PersistAnyFn(key, value)
		return
	}
	h.writeSettingsFileKey(key, value)
}

func (h *Handler) writeSettingsFileKey(key string, value any) {
	if h.cfg.StateFile == "" || key == "" {
		return
	}
	p := filepath.Join(filepath.Dir(h.cfg.StateFile), "settings.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	m := make(map[string]any)
	if raw, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	m[key] = value
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, p)
		}
	}
}

func (h *Handler) getAuthDir() string {
	if h != nil {
		if h.cfg.AuthDir != "" {
			return h.cfg.AuthDir
		}
		if h.cfg.StateFile != "" {
			return filepath.Dir(h.cfg.StateFile)
		}
	}
	return "./auths"
}

func (h *Handler) registerRoutes() {
	// AI Inference & Compatible Protocols (IP Whitelist + Brute-force lockout + Bearer/x-api-key RBAC)
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.anthropicCountTokens))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responsesAPI))

	// Status, Metrics & Health
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))

	// Pool Admin APIs
	if h.cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAuth(h.adminAccountDisable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAuth(h.adminAccountEnable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/revive", h.withAuth(h.adminAccountRevive))
	}

	// Portal, Console & Web Management APIs
	h.RegisterWebUI()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowed, clientIP := h.IsIPAllowed(r)
		if !allowed {
			log.Printf("WARN: [security] IP blocked by whitelist: ip=%s method=%s path=%s", clientIP, r.Method, r.URL.Path)
			writeOpenAIError(w, http.StatusForbidden, "ip_not_allowed", "access denied: client IP is not in whitelist")
			return
		}
		if globalAuthLimiter.isAPIKeyLocked(clientIP) {
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many invalid API key attempts, try again later")
			return
		}
		currentKey := h.GetAPIKey()
		provided := extractAPIToken(r)

		// 1. Multi-user API key validation (RBAC + RPM + daily quota)
		if h.cfg.UserMgr != nil && provided != "" {
			if u, ok := h.cfg.UserMgr.FindByAPIKey(provided); ok {
				globalAuthLimiter.resetAPIKeyFail(clientIP)
				if err := h.cfg.UserMgr.CheckRPM(provided); err != nil {
					writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error())
					return
				}
				if err := h.cfg.UserMgr.CheckDailyCredit(provided); err != nil {
					writeOpenAIError(w, http.StatusForbidden, "quota_exceeded", err.Error())
					return
				}
				ctx := context.WithValue(r.Context(), userContextKey, u)
				next(w, r.WithContext(ctx))
				return
			}
		}

		// 2. Master/legacy API key check
		if currentKey != "" {
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(currentKey)) != 1 {
				globalAuthLimiter.recordAPIKeyFail(clientIP)
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}

		globalAuthLimiter.resetAPIKeyFail(clientIP)
		if h.cfg.UserMgr != nil {
			if adminUser, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
				ctx := context.WithValue(r.Context(), userContextKey, adminUser)
				next(w, r.WithContext(ctx))
				return
			}
		}
		next(w, r)
	}
}

func extractAPIToken(r *http.Request) string {
	if authz := strings.TrimSpace(r.Header.Get("Authorization")); authz != "" {
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			return strings.TrimSpace(authz[7:])
		}
		return authz
	}
	if xk := strings.TrimSpace(r.Header.Get("x-api-key")); xk != "" {
		return xk
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

func (h *Handler) requestOwner(r *http.Request) string {
	if r == nil {
		return "public"
	}
	if u, ok := r.Context().Value(userContextKey).(*usermgr.User); ok && u != nil && u.Username != "" {
		return u.Username
	}
	return "public"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string, extra ...string) {
	errObj := map[string]any{
		"message": message,
		"type":    code,
		"code":    code,
	}
	if len(extra) > 0 && extra[0] != "" {
		errObj["gateway_hint"] = extra[0]
	}
	writeJSON(w, status, map[string]any{"error": errObj})
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func init() {
	if os.Getenv("QD_ENSURE_DATA") == "1" {
		if err := os.MkdirAll("./data", 0o755); err != nil {
			log.Printf("WARN: mkdir ./data: %v", err)
		}
	}
}
