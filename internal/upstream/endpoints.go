// Package upstream implements the Qoder dual-realm (CN & Global) HTTP/SSE client,
// COSY signing, OAuth/PAT token lifecycle, model catalog, error classification,
// quota breakdown, campaign check-in, and inference payload adaptation.
package upstream

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

const (
	RealmCN     = "cn"
	RealmGlobal = "global"

	VPCDomainSuffix = "vpc.qoder.com.cn"

	ClientIDCN     = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	ClientIDGlobal = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

	RedirectURICN = "qoder-work-cn://"

	PathChatStream      = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	PathModelList       = "/algo/api/v2/model/list?Encode=1"
	PathUserInfo        = "/api/v1/userinfo"
	PathDeviceSelect    = "/device/selectAccounts"
	PathDevicePoll      = "/api/v1/deviceToken/poll"
	PathDeviceRefresh   = "/api/v1/deviceToken/refresh"
	PathJobExchange     = "/api/v1/jobToken/exchange"
	PathJobRefresh      = "/api/v1/jobToken/refresh"
	PathJobTokenV3      = "/algo/api/v3/user/jobToken?Encode=1"
	PathCampaigns       = "/sash/api/v1/me/campaigns"
	PathCampaignClaim   = "/sash/api/v1/me/campaigns/%s/claim"
	PathCampaignReward  = "/sash/api/v1/me/campaigns/%s/reward"
	PathProEligibility  = "/sash/api/v1/me/pro-upgrade/eligibility"
	PathProClaim        = "/sash/api/v1/me/pro-upgrade/claim"
	DefaultCreditsPath  = "/api/v2/quota/usage"
	CreditsSourceFile   = "credits-source.json"
	defaultEndpointCool = 90 * time.Second
)

// CreditsCandidatePaths lists candidate Qoder quota endpoints probed across realms.
var CreditsCandidatePaths = []string{
	"/api/v2/quota/usage",
	"/api/v1/quota/usage",
	"/api/v2/quota",
	"/api/v2/user/quota",
	"/api/v2/me/quota",
	"/api/v2/credits/usage",
	"/api/v2/credits",
	"/api/v2/billing/quota",
	"/sash/api/v1/me/quota",
	"/sash/api/v1/me/credits",
	"/sash/api/v1/me/usage",
}

// RealmConfig captures endpoints and OAuth constants for one Qoder deployment realm.
type RealmConfig struct {
	Realm           string
	Website         string
	OpenAPI         string
	Gateways        []string
	ClientID        string
	RedirectURI     string
	SendRedirectURI bool
	SendClientID    bool
	NonceDashed     bool
	UserAgent       string
	VPCInstance     string
}

// DefaultRealmConfig returns official CN or Global endpoints.
func DefaultRealmConfig(realm string) RealmConfig {
	if NormalizeRealm(realm) == RealmCN {
		return RealmConfig{
			Realm:           RealmCN,
			Website:         "https://qoder.com.cn",
			OpenAPI:         "https://openapi.qoder.com.cn",
			Gateways:        []string{"https://gateway.qoder.com.cn"},
			ClientID:        ClientIDCN,
			RedirectURI:     RedirectURICN,
			SendRedirectURI: true,
			SendClientID:    true,
			NonceDashed:     true,
			UserAgent:       "QoderWork/1.1.75",
		}
	}
	return RealmConfig{
		Realm:           RealmGlobal,
		Website:         "https://qoder.com",
		OpenAPI:         "https://openapi.qoder.sh",
		Gateways:        []string{"https://api3.qoder.sh", "https://api2.qoder.sh", "https://api1.qoder.sh"},
		ClientID:        ClientIDGlobal,
		SendRedirectURI: false,
		SendClientID:    true,
		NonceDashed:     false,
		UserAgent:       "Qoder/1.1.75",
	}
}

// NormalizeRealm canonicalizes realm strings ("intl" -> "global").
func NormalizeRealm(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "cn", "china", "domestic":
		return RealmCN
	case "intl", "global", "overseas", "en":
		return RealmGlobal
	default:
		return RealmGlobal
	}
}

// ParseVPCEndpoint parses a CN enterprise VPC instance or hostname into three official hosts.
func ParseVPCEndpoint(raw string) (instance string, website string, openapi string, gateway string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", "", nil
	}
	if raw != trimmed || strings.ContainsAny(raw, "\r\n\t") {
		return "", "", "", "", fmt.Errorf("invalid VPC endpoint: contains whitespace or control characters")
	}
	target := raw
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	u, perr := url.Parse(target)
	if perr != nil || u.Hostname() == "" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", "", "", fmt.Errorf("invalid VPC endpoint: expected instance name or *.%s host", VPCDomainSuffix)
	}
	host := strings.ToLower(u.Hostname())
	inst := host
	if strings.HasSuffix(host, "."+VPCDomainSuffix) {
		prefix := strings.TrimSuffix(host, "."+VPCDomainSuffix)
		for _, suffix := range []string{"-openapi", "-gateway"} {
			if strings.HasSuffix(prefix, suffix) && len(prefix) > len(suffix) {
				prefix = strings.TrimSuffix(prefix, suffix)
				break
			}
		}
		if prefix == "" || strings.Contains(prefix, ".") {
			return "", "", "", "", fmt.Errorf("invalid VPC endpoint: malformed VPC prefix")
		}
		inst = prefix
	} else if strings.Contains(host, ".") {
		return "", "", "", "", fmt.Errorf("invalid VPC endpoint: domain must end with %s", VPCDomainSuffix)
	}
	return inst,
		"https://" + inst + "." + VPCDomainSuffix,
		"https://" + inst + "-openapi." + VPCDomainSuffix,
		"https://" + inst + "-gateway." + VPCDomainSuffix,
		nil
}

type endpointHealth struct {
	fails         int
	successes     int
	coolingUntil  time.Time
	lastFailAt    time.Time
	lastSuccessAt time.Time
	lastErr       string
}

// EndpointRegistry tracks failover health across candidate gateway/openapi hosts.
type EndpointRegistry struct {
	mu      sync.Mutex
	entries map[string]*endpointHealth
}

// EndpointStatus summarizes health of one endpoint candidate.
type EndpointStatus struct {
	Purpose    string `json:"purpose"`
	URL        string `json:"url"`
	Fails      int    `json:"fails"`
	Successes  int    `json:"successes"`
	CoolingMS  int64  `json:"cooling_ms"`
	LastFailAt string `json:"last_fail_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

func newEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{entries: make(map[string]*endpointHealth)}
}

func epKey(purpose, rawURL string) string {
	return purpose + "|" + rawURL
}

func (r *EndpointRegistry) noteSuccess(purpose, rawURL string) {
	if r == nil || rawURL == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := epKey(purpose, rawURL)
	h := r.entries[k]
	if h == nil {
		h = &endpointHealth{}
		r.entries[k] = h
	}
	h.successes++
	h.fails = 0
	h.coolingUntil = time.Time{}
	h.lastSuccessAt = time.Now()
	h.lastErr = ""
}

func (r *EndpointRegistry) noteFailure(purpose, rawURL, errMsg string) {
	if r == nil || rawURL == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := epKey(purpose, rawURL)
	h := r.entries[k]
	if h == nil {
		h = &endpointHealth{}
		r.entries[k] = h
	}
	h.fails++
	h.lastFailAt = time.Now()
	h.lastErr = errMsg
	cool := defaultEndpointCool
	if h.fails >= 3 {
		cool = 3 * time.Minute
	}
	h.coolingUntil = h.lastFailAt.Add(cool)
}

func (r *EndpointRegistry) ordered(purpose string, candidates []string) []string {
	if r == nil || len(candidates) <= 1 {
		return append([]string(nil), candidates...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var healthy, cooling []string
	for _, u := range candidates {
		h := r.entries[epKey(purpose, u)]
		if h != nil && now.Before(h.coolingUntil) {
			cooling = append(cooling, u)
		} else {
			healthy = append(healthy, u)
		}
	}
	return append(healthy, cooling...)
}

// Snapshots returns health snapshots for all tracked endpoints.
func (r *EndpointRegistry) Snapshots() []EndpointStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]EndpointStatus, 0, len(r.entries))
	for k, h := range r.entries {
		idx := strings.IndexByte(k, '|')
		purpose, rawURL := "", k
		if idx >= 0 {
			purpose, rawURL = k[:idx], k[idx+1:]
		}
		st := EndpointStatus{
			Purpose:   purpose,
			URL:       rawURL,
			Fails:     h.fails,
			Successes: h.successes,
			LastError: h.lastErr,
		}
		if now.Before(h.coolingUntil) {
			st.CoolingMS = h.coolingUntil.Sub(now).Milliseconds()
		}
		if !h.lastFailAt.IsZero() {
			st.LastFailAt = h.lastFailAt.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}

// Client manages all outbound Qoder API interactions across CN and Global realms.
type Client struct {
	HTTP          *http.Client
	Sessions      *cosy.SessionCache
	Endpoints     *EndpointRegistry
	DataDir       string
	PassthroughIP bool

	// Test and deployment overrides; empty uses official Qoder endpoints.
	APIBaseCN     string
	APIBaseGlobal string
	OpenAPICN     string
	OpenAPIGlobal string
	WebsiteCN     string
	WebsiteGlobal string

	// GlobalVPC allows cluster-wide CN VPC endpoint fallback when account has none.
	GlobalVPC string

	creditsSrcMu sync.Mutex
	creditsSrc   map[string]creditsSourceEntry

	activityHookMu sync.RWMutex
	activityHook   func(kind string, ok bool, uid, nickname, realm, detail string, amount int)
}

// New constructs a default production Qoder upstream Client.
func New() *Client {
	return NewWithProxy("")
}

// NewWithProxy constructs a production Qoder upstream Client with optional HTTP/HTTPS/SOCKS5 proxy URL.
// Empty proxyURL falls back to standard environment proxy settings (HTTP_PROXY/HTTPS_PROXY).
func NewWithProxy(proxyURL string) *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   25,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DisableCompression:    false,
		Proxy:                 http.ProxyFromEnvironment,
	}
	if p := strings.TrimSpace(proxyURL); p != "" {
		if u, err := url.Parse(p); err != nil {
			log.Printf("WARN: [upstream] invalid proxy URL %q, falling back to environment proxy: %v", p, err)
		} else {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &Client{
		HTTP: &http.Client{
			Timeout:   600 * time.Second,
			Transport: tr,
		},
		Sessions:  cosy.NewSessionCache(),
		Endpoints: newEndpointRegistry(),
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

var defaultSharedSessions = cosy.NewSessionCache()

func (c *Client) sessionCache() *cosy.SessionCache {
	if c != nil && c.Sessions != nil {
		return c.Sessions
	}
	return defaultSharedSessions
}

// SetActivityHook registers a callback for recording checkin/pro-upgrade events.
func (c *Client) SetActivityHook(fn func(kind string, ok bool, uid, nickname, realm, detail string, amount int)) {
	if c == nil {
		return
	}
	c.activityHookMu.Lock()
	c.activityHook = fn
	c.activityHookMu.Unlock()
}

func (c *Client) emitActivity(kind string, ok bool, uid, nickname, realm, detail string, amount int) {
	if c == nil {
		return
	}
	c.activityHookMu.RLock()
	fn := c.activityHook
	c.activityHookMu.RUnlock()
	if fn != nil {
		fn(kind, ok, uid, nickname, realm, detail, amount)
	}
}

// RealmConfigForAccount returns effective realm configuration with Client/VPC overrides applied.
func (c *Client) RealmConfigForAccount(a *auth.Auth) RealmConfig {
	realm := RealmGlobal
	vpc := ""
	if a != nil {
		realm = NormalizeRealm(a.Realm())
		vpc = a.VPCEndpointValue()
	}
	if vpc == "" && c != nil && realm == RealmCN {
		vpc = c.GlobalVPC
	}
	cfg := DefaultRealmConfig(realm)
	if realm == RealmCN && vpc != "" {
		if inst, web, oapi, gw, err := ParseVPCEndpoint(vpc); err == nil && inst != "" {
			cfg.VPCInstance = inst
			cfg.Website = web
			cfg.OpenAPI = oapi
			cfg.Gateways = []string{gw}
		}
	}
	if c != nil {
		if realm == RealmCN {
			if c.APIBaseCN != "" {
				cfg.Gateways = []string{strings.TrimRight(c.APIBaseCN, "/")}
			}
			if c.OpenAPICN != "" {
				cfg.OpenAPI = strings.TrimRight(c.OpenAPICN, "/")
			}
			if c.WebsiteCN != "" {
				cfg.Website = strings.TrimRight(c.WebsiteCN, "/")
			}
		} else {
			if c.APIBaseGlobal != "" {
				cfg.Gateways = []string{strings.TrimRight(c.APIBaseGlobal, "/")}
			}
			if c.OpenAPIGlobal != "" {
				cfg.OpenAPI = strings.TrimRight(c.OpenAPIGlobal, "/")
			}
			if c.WebsiteGlobal != "" {
				cfg.Website = strings.TrimRight(c.WebsiteGlobal, "/")
			}
		}
	}
	return cfg
}

// OrderedGateways returns candidate gateway bases ordered by recent health.
func (c *Client) OrderedGateways(a *auth.Auth) []string {
	cfg := c.RealmConfigForAccount(a)
	if c == nil || c.Endpoints == nil {
		return cfg.Gateways
	}
	return c.Endpoints.ordered("chat-"+cfg.Realm, cfg.Gateways)
}

// EndpointStatuses exposes health tracking snapshots for diagnostics.
func (c *Client) EndpointStatuses() []EndpointStatus {
	if c == nil || c.Endpoints == nil {
		return nil
	}
	return c.Endpoints.Snapshots()
}

// ExtractClientIP extracts client IP for optional passthrough forwarding.
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if parts := strings.Split(xff, ","); len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	addr := strings.TrimSpace(r.RemoteAddr)
	if idx := strings.LastIndexByte(addr, ':'); idx > 0 {
		if !strings.HasPrefix(addr, "[") || strings.Contains(addr[:idx], "]") {
			return strings.Trim(addr[:idx], "[]")
		}
	}
	return addr
}
