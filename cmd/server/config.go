package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qoder2api/internal/config"
	"qoder2api/internal/prompt"
)

// Config defines top-level configuration loaded from config.json, persisted settings.json, and env vars.
type Config struct {
	Listen             string   `json:"listen"`               // default ":7865"
	APIKey             string   `json:"api_key"`              // empty = open (unless Admin.Enabled)
	WebPassword        string   `json:"web_password"`         // dashboard login password
	IPWhitelistEnabled bool     `json:"ip_whitelist_enabled"` // restrict API routes by CIDR/IP
	AllowedIPs         []string `json:"allowed_ips"`
	AllowLocalhost     *bool    `json:"allow_localhost,omitempty"`
	AllowRemoteAdmin   bool     `json:"allow_remote_admin"`
	AuthDir            string   `json:"auth_dir"`   // "./auths"
	StateFile          string   `json:"state_file"` // "./data/state.json"

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`     // "3m"
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	Admin struct {
		Enabled bool `json:"enabled"`
	} `json:"admin"`

	Global struct {
		Enabled       bool   `json:"enabled"`
		APIBaseCN     string `json:"api_base_cn"`
		APIBaseGlobal string `json:"api_base_global"`
		OpenAPICN     string `json:"openapi_cn"`
		OpenAPIGlobal string `json:"openapi_global"`
		WebsiteCN     string `json:"website_cn"`
		WebsiteGlobal string `json:"website_global"`
		VPC           string `json:"vpc"`
	} `json:"global"`

	Upstream struct {
		TimeoutSeconds       int    `json:"timeout_seconds"`        // total client timeout (600s for long streams)
		HeaderTimeoutSeconds int    `json:"header_timeout_seconds"` // response header timeout (120s)
		PassthroughIP        bool   `json:"passthrough_ip"`
		Proxy                string `json:"proxy"`
		VPCEndpoint          string `json:"vpc_endpoint"`
		WaitMode             bool   `json:"queue_wait_mode"`
		MaxQueueRetries      int    `json:"queue_max_retries"`
		WaitCapSeconds       int    `json:"queue_wait_cap_seconds"`
		WaitDefaultSeconds   int    `json:"queue_wait_default_seconds"`
		WaitFirstToken       bool   `json:"queue_wait_first_token"`
		QueueBudgetSeconds   int    `json:"queue_budget_seconds"`
	} `json:"upstream"`

	Prompt struct {
		Mode string `json:"mode"` // "passthrough" | "custom" | "append"
		File string `json:"file"`
	} `json:"prompt"`

	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"upstash"`

	Pool struct {
		MaxInFlight         int     `json:"max_in_flight"`
		MaxInFlightGlobal   int     `json:"max_in_flight_global"`
		BreakerThreshold    int     `json:"breaker_threshold"`
		BreakerCooldown     string  `json:"breaker_cooldown"`
		BreakerCooldownMax  string  `json:"breaker_cooldown_max"`
		DegradeThreshold    int     `json:"degrade_threshold"`
		DegradeCooldown     string  `json:"degrade_cooldown"`
		DegradeCooldownMax  string  `json:"degrade_cooldown_max"`
		IdleWeightPerHour   float64 `json:"idle_weight_per_hour"`
		IdleWeightMax       float64 `json:"idle_weight_max"`
		ExpiringSoon        string  `json:"expiring_soon"`
		CostExploreInterval string  `json:"cost_explore_interval"`
		MaxRotate           int     `json:"max_rotate"`
	} `json:"pool"`

	ModelAliases map[string]string   `json:"model_aliases,omitempty"`
	ModelRoutes  map[string][]string `json:"model_routes,omitempty"`
	FreeModels   []string            `json:"free_models,omitempty"`

	Alert struct {
		Enabled   bool     `json:"enabled"`
		Webhooks  []string `json:"webhooks"`
		Events    []string `json:"events"`
		MinCredit int      `json:"min_credit"`
	} `json:"alert"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`
		TTL        string `json:"ttl"`
		GCInterval string `json:"gc_interval"`
	} `json:"session_sticky"`

	// Parsed durations populated by normalize().
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	ExpiringSoonDur        time.Duration `json:"-"`
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default returns a ready-to-run Qoder2Api configuration on :7865.
func Default() *Config {
	c := &Config{
		Listen:    ":7865",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "3m"
	c.Cooldown.SoftRateMax = "2h"
	c.Schedule = config.DefaultSchedule()

	c.Global.Enabled = true

	c.Upstream.TimeoutSeconds = 600
	c.Upstream.HeaderTimeoutSeconds = 120
	c.Upstream.WaitMode = true
	c.Upstream.MaxQueueRetries = 4
	c.Upstream.WaitCapSeconds = 30
	c.Upstream.WaitDefaultSeconds = 5
	c.Upstream.WaitFirstToken = true
	c.Upstream.QueueBudgetSeconds = 120

	c.Prompt.Mode = "passthrough"

	c.Pool.MaxInFlight = 3
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h"
	c.Pool.CostExploreInterval = "30m"
	c.Pool.MaxRotate = 8

	c.ModelAliases = map[string]string{
		"gpt-4o":           "smodel",
		"claude-sonnet":    "smodel",
		"claude-3-7-sonnet": "smodel",
		"deepseek-chat":    "cn:glm-5.2",
	}
	c.FreeModels = []string{"auto-free", "q35fmodel", "g3flmodel", "q3fmodel"}

	c.Alert.MinCredit = 100

	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load reads config from JSON (if path non-empty), overlays ./data/settings.json, then applies env overrides.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	if c.StateFile != "" {
		settingsFile := filepath.Join(filepath.Dir(c.StateFile), "settings.json")
		if raw, err := os.ReadFile(settingsFile); err == nil {
			var s struct {
				APIKey             string   `json:"api_key"`
				WebPassword        string   `json:"web_password"`
				IPWhitelistEnabled *bool    `json:"ip_whitelist_enabled"`
				AllowedIPs         []string `json:"allowed_ips"`
				AllowLocalhost     *bool    `json:"allow_localhost"`
				AllowRemoteAdmin   *bool    `json:"allow_remote_admin"`
				GlobalVPC          string   `json:"global_vpc"`
			}
			if json.Unmarshal(raw, &s) == nil {
				if s.APIKey != "" {
					c.APIKey = s.APIKey
				}
				if s.WebPassword != "" {
					c.WebPassword = s.WebPassword
				}
				if s.IPWhitelistEnabled != nil {
					c.IPWhitelistEnabled = *s.IPWhitelistEnabled
				}
				if s.AllowedIPs != nil {
					c.AllowedIPs = s.AllowedIPs
				}
				if s.AllowLocalhost != nil {
					v := *s.AllowLocalhost
					c.AllowLocalhost = &v
				}
				if s.AllowRemoteAdmin != nil {
					c.AllowRemoteAdmin = *s.AllowRemoteAdmin
				}
				if s.GlobalVPC != "" {
					c.Global.VPC = s.GlobalVPC
				}
			}
		}
	}

	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func parseBoolEnv(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
}

func applyEnv(c *Config) {
	if v := envFirst("QD2A_LISTEN", "QODER2API_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := envFirst("QD2A_API_KEY", "QODER2API_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := envFirst("QD2A_WEB_PASSWORD", "QODER2API_WEB_PASSWORD"); v != "" {
		c.WebPassword = v
	}
	if v := envFirst("QD2A_ALLOWED_IPS", "QODER2API_ALLOWED_IPS"); v != "" {
		parts := strings.Split(v, ",")
		ips := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				ips = append(ips, s)
			}
		}
		c.AllowedIPs = ips
		if len(ips) > 0 && envFirst("QD2A_IP_WHITELIST_ENABLED", "QODER2API_IP_WHITELIST_ENABLED") == "" {
			c.IPWhitelistEnabled = true
		}
	}
	if v := envFirst("QD2A_IP_WHITELIST_ENABLED", "QODER2API_IP_WHITELIST_ENABLED"); v != "" {
		c.IPWhitelistEnabled = parseBoolEnv(v)
	}
	if v := envFirst("QD2A_ALLOW_LOCALHOST", "QODER2API_ALLOW_LOCALHOST"); v != "" {
		b := parseBoolEnv(v)
		c.AllowLocalhost = &b
	}
	if v := envFirst("QD2A_ALLOW_REMOTE_ADMIN", "QODER2API_ALLOW_REMOTE_ADMIN"); v != "" {
		c.AllowRemoteAdmin = parseBoolEnv(v)
	}
	if v := envFirst("QD2A_AUTH_DIR", "QODER2API_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := envFirst("QD2A_STATE_FILE", "QODER2API_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := envFirst("QD2A_SOFT_RATE", "QODER2API_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := envFirst("QD2A_SOFT_RATE_MAX", "QODER2API_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := envFirst("QD2A_TIMEOUT_SECONDS", "QODER2API_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := envFirst("QD2A_HEADER_TIMEOUT_SECONDS", "QODER2API_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := envFirst("QD2A_PASSTHROUGH_IP", "QODER2API_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := envFirst("QD2A_UPSTREAM_PROXY", "QD2A_PROXY", "QODER2API_PROXY"); v != "" {
		c.Upstream.Proxy = v
	}
	if v := envFirst("QD2A_GLOBAL_ENABLED", "QODER2API_GLOBAL_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Global.Enabled = b
		}
	}
	if v := envFirst("QD2A_GLOBAL_VPC", "QODER2API_GLOBAL_VPC"); v != "" {
		c.Global.VPC = v
	}
	if v := envFirst("QD2A_PROMPT_MODE", "QODER2API_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := envFirst("QD2A_PROMPT_FILE", "QODER2API_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := envFirst("QD2A_EXPIRING_SOON", "QODER2API_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := envFirst("QD2A_ADMIN_ENABLED", "QODER2API_ADMIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.Enabled = b
		}
	}
	if v := envFirst("QD2A_UPSTASH_URL", "UPSTASH_REDIS_REST_URL"); v != "" {
		c.Upstash.URL = v
	}
	if v := envFirst("QD2A_UPSTASH_TOKEN", "UPSTASH_REDIS_REST_TOKEN"); v != "" {
		c.Upstash.Token = v
	}
	if v := envFirst("QD2A_ALERT_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Alert.Enabled = b
		}
	}
	if v := envFirst("QD2A_ALERT_WEBHOOKS"); v != "" {
		var list []string
		for _, p := range strings.Split(v, ",") {
			if s := strings.TrimSpace(p); s != "" {
				list = append(list, s)
			}
		}
		c.Alert.Webhooks = list
	}
	if v := envFirst("QD2A_MODEL_ALIASES"); v != "" {
		var m map[string]string
		if json.Unmarshal([]byte(v), &m) == nil {
			c.ModelAliases = m
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = 8
	}
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
	}
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 600
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = 120
	}
	if c.Global.VPC == "" && c.Upstream.VPCEndpoint != "" {
		c.Global.VPC = c.Upstream.VPCEndpoint
	}
	if c.Alert.MinCredit <= 0 {
		c.Alert.MinCredit = 100
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true requires a non-empty api_key; configure api_key or set admin.enabled=false")
	}
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

func (c *Config) normalizePrompt() error {
	switch strings.ToLower(strings.TrimSpace(c.Prompt.Mode)) {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q invalid (must be passthrough, custom, or append)", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
