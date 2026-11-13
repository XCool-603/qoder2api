package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"qoder2api/internal/auth"
)

// QuotaBucket represents a single normalized quota bucket from Qoder APIs.
type QuotaBucket struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"` // plan / pack / sota / shared
	Remain     float64 `json:"remain"`
	Used       float64 `json:"used"`
	Size       float64 `json:"size"`
	ExpiresAt  int64   `json:"expires_at,omitempty"`
	Unlimited  bool    `json:"unlimited,omitempty"`
	Available  bool    `json:"available"`
	NoExpiry   bool    `json:"no_expiry,omitempty"`
	DaysLeft   int     `json:"days_left,omitempty"`
	ExpireDate string  `json:"expire_date,omitempty"`
}

// ExpiringCreditsDetail summarizes soon-expiring credit slice for workbuddy2api-aligned pool scheduling.
type ExpiringCreditsDetail struct {
	Total       float64       `json:"total"`
	Expiring    float64       `json:"expiring"`
	SoonWindow  time.Duration `json:"soon_window"`
	Buckets     []QuotaBucket `json:"buckets,omitempty"`
	SharedPool  *QuotaBucket  `json:"shared,omitempty"`
	Degraded    bool          `json:"degraded,omitempty"`
	SourcePath  string        `json:"source_path,omitempty"`
	Composition []QuotaBucket `json:"composition,omitempty"`
}

// QuotaBreakdown represents Qoder credits breakdown across plan/addon/sota/shared pools.
type QuotaBreakdown struct {
	Remain          int64         `json:"remain"`
	RemainEffective int64         `json:"remain_effective"`
	Used            int64         `json:"used"`
	Size            int64         `json:"size"`
	ExpiringSoon    int64         `json:"expiring_soon"`
	Exceeded        bool          `json:"exceeded"`
	UsagePct        float64       `json:"usage_pct,omitempty"`
	ExpiresAt       int64         `json:"expires_at,omitempty"`
	Shared          *QuotaBucket  `json:"shared,omitempty"`
	Packages        []QuotaBucket `json:"packages,omitempty"`
	Composition     []QuotaBucket `json:"composition,omitempty"`
	Fields          []string      `json:"fields,omitempty"`
	SourceHost      string        `json:"source_host,omitempty"`
	SourcePath      string        `json:"source_path,omitempty"`
	Degraded        bool          `json:"degraded,omitempty"`
}

type creditsSourceEntry struct {
	Host   string   `json:"host"`
	Path   string   `json:"path"`
	Fields []string `json:"fields,omitempty"`
	At     float64  `json:"at"`
}

// ProbeRow captures the result of testing one candidate quota endpoint.
type ProbeRow struct {
	URL    string   `json:"url"`
	Host   string   `json:"host"`
	Path   string   `json:"path"`
	Status int      `json:"status"`
	OK     bool     `json:"ok"`
	Fields []string `json:"fields,omitempty"`
	Note   string   `json:"note,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// CreditsProbeResult reports discovery across Qoder's multi-host quota endpoints.
type CreditsProbeResult struct {
	OK    bool            `json:"ok"`
	Realm string          `json:"realm"`
	Rows  []ProbeRow      `json:"rows"`
	Hit   *QuotaBreakdown `json:"hit,omitempty"`
}

var quotaBucketSpecs = []struct {
	keys  map[string]bool
	kind  string
	label string
}{
	{
		keys: map[string]bool{
			"userquota": true, "planquota": true, "subscriptionquota": true, "basequota": true, "plan": true,
		},
		kind: "plan", label: "套餐内 Credits",
	},
	{
		keys: map[string]bool{
			"sota": true, "sotacredits": true, "sotaquota": true, "sotaspecial": true,
			"dedicatedresourcepackages": true, "dedicatedresourcepackage": true,
			"exclusivepackages": true, "sotapackages": true,
		},
		kind: "sota", label: "SOTA 专属积分",
	},
	{
		keys: map[string]bool{
			"addonquota": true, "resourcepackage": true, "resourcepackages": true,
			"addon": true, "package": true, "packages": true, "bonus": true,
		},
		kind: "pack", label: "资源包",
	},
	{
		keys: map[string]bool{
			"orgresourcepackage": true, "sharedquota": true, "teamquota": true, "poolquota": true, "sharedcredits": true,
		},
		kind: "shared", label: "共享额度池",
	},
}

func (c *Client) creditsSourcePath() string {
	if c == nil || c.DataDir == "" {
		return ""
	}
	return filepath.Join(c.DataDir, CreditsSourceFile)
}

func (c *Client) loadCreditsSources() map[string]creditsSourceEntry {
	if c == nil {
		return nil
	}
	c.creditsSrcMu.Lock()
	defer c.creditsSrcMu.Unlock()
	if c.creditsSrc != nil {
		return c.creditsSrc
	}
	m := make(map[string]creditsSourceEntry)
	p := c.creditsSourcePath()
	if p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(raw, &m)
		}
	}
	c.creditsSrc = m
	return c.creditsSrc
}

func (c *Client) saveCreditsSource(realm, host, path string, fields []string) {
	if c == nil {
		return
	}
	c.creditsSrcMu.Lock()
	defer c.creditsSrcMu.Unlock()
	if c.creditsSrc == nil {
		c.creditsSrc = make(map[string]creditsSourceEntry)
	}
	c.creditsSrc[realm] = creditsSourceEntry{
		Host: host, Path: path, Fields: fields, At: float64(time.Now().Unix()),
	}
	if p := c.creditsSourcePath(); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if b, err := json.MarshalIndent(c.creditsSrc, "", "  "); err == nil {
			tmp := p + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, p)
			}
		}
	}
}

func extractBucket(src any, kind, defaultName string) (QuotaBucket, bool) {
	if f, ok := toFloat(src); ok {
		return QuotaBucket{
			Name: defaultName, Kind: kind, Remain: f, Size: f, Available: true,
		}, true
	}
	m, ok := src.(map[string]any)
	if !ok {
		return QuotaBucket{}, false
	}
	remain, hasRemain := firstFloat(m, "remaining", "remain", "balance", "availableBalance")
	used, _ := firstFloat(m, "used", "consumed")
	size, hasSize := 0.0, false
	if kind == "shared" {
		if s, ok := toFloat(m["cap"]); ok {
			size, hasSize = s, true
		}
	}
	if !hasSize {
		size, hasSize = firstFloat(m, "total", "cap", "size", "quota")
	}
	if !hasSize && hasRemain {
		size = remain + used
		hasSize = true
	}
	if !hasRemain && hasSize {
		remain = size - used
		if remain < 0 {
			remain = 0
		}
		hasRemain = true
	}
	if !hasRemain && !hasSize {
		return QuotaBucket{}, false
	}
	avail := true
	if rawAv, ok := m["available"]; ok {
		if b, ok := rawAv.(bool); ok {
			avail = b
		}
	}
	label := extractDisplayLabel(m)
	if label == "" {
		label = defaultName
	}
	exp, _ := firstEpoch(m, "expiresAt", "expireAt", "expires_at", "expiredAt", "validUntil", "dueAt")
	daysLeft := anyInt(m["daysLeft"])
	if daysLeft == 0 {
		daysLeft = anyInt(m["days_left"])
	}
	noExp, _ := m["noExpiry"].(bool)
	unlimited := kind == "shared" && size < 0
	var expireDate string
	if exp > 0 {
		expireDate = time.Unix(exp, 0).Format("2006-01-02")
	}
	return QuotaBucket{
		Name:       label,
		Kind:       kind,
		Remain:     remain,
		Used:       used,
		Size:       size,
		ExpiresAt:  exp,
		Unlimited:  unlimited,
		Available:  avail,
		NoExpiry:   noExp,
		DaysLeft:   daysLeft,
		ExpireDate: expireDate,
	}, true
}

func extractDisplayLabel(m map[string]any) string {
	if dls, ok := m["displayLabels"].([]any); ok {
		for _, item := range dls {
			dl, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(anyStr(dl["dimension"])) != "title" {
				continue
			}
			if i18n, ok := dl["valueI18n"].(map[string]any); ok {
				for _, lang := range []string{"zh-CN", "en-US"} {
					if v := strings.TrimSpace(anyStr(i18n[lang])); v != "" {
						return v
					}
				}
			}
			if v := strings.TrimSpace(anyStr(dl["value"])); v != "" {
				return v
			}
		}
	}
	return strings.TrimSpace(anyStr(m["name"]))
}

func firstFloat(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if raw, exists := m[k]; exists {
			if f, ok := toFloat(raw); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func firstEpoch(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if raw, exists := m[k]; exists {
			if ep, ok := parseEpochFlex(raw); ok {
				return ep, true
			}
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// NormalizeQuotaPayload performs BFS traversal across any Qoder quota response format.
func NormalizeQuotaPayload(payload any, realm string) (*QuotaBreakdown, bool) {
	if payload == nil {
		return nil, false
	}
	type qnode struct {
		node any
	}
	queue := []qnode{{node: payload}}
	var found []QuotaBucket
	type sig struct {
		kind   string
		size   int64
		remain int64
		exp    int64
	}
	seen := make(map[sig]bool)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		switch v := cur.node.(type) {
		case map[string]any:
			for k, val := range v {
				low := strings.ToLower(k)
				var matchKind, matchLabel string
				for _, sp := range quotaBucketSpecs {
					if sp.keys[low] {
						matchKind, matchLabel = sp.kind, sp.label
						break
					}
				}
				if matchKind != "" && val != nil {
					var items []any
					if arr, ok := val.([]any); ok {
						items = arr
					} else {
						items = []any{val}
					}
					for idx, item := range items {
						if b, ok := extractBucket(item, matchKind, matchLabel); ok {
							if len(items) > 1 {
								b.Name = fmt.Sprintf("%s #%d", b.Name, idx+1)
							}
							s := sig{kind: b.Kind, size: int64(b.Size), remain: int64(b.Remain), exp: b.ExpiresAt}
							if !seen[s] {
								seen[s] = true
								found = append(found, b)
							}
						}
					}
				}
				if _, isMap := val.(map[string]any); isMap {
					queue = append(queue, qnode{node: val})
				} else if _, isArr := val.([]any); isArr {
					queue = append(queue, qnode{node: val})
				}
			}
		case []any:
			for _, item := range v {
				if _, isMap := item.(map[string]any); isMap {
					queue = append(queue, qnode{node: item})
				} else if _, isArr := item.([]any); isArr {
					queue = append(queue, qnode{node: item})
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, false
	}

	fieldSet := make(map[string]bool)
	var sumRemain, sumUsed, sumSize float64
	var shared *QuotaBucket
	var pkgs []QuotaBucket
	for i := range found {
		b := found[i]
		fieldSet[b.Kind] = true
		if b.Kind == "shared" {
			cp := b
			shared = &cp
			continue
		}
		pkgs = append(pkgs, b)
		if b.Available {
			sumRemain += b.Remain
			sumUsed += b.Used
			sumSize += b.Size
		}
	}

	fields := make([]string, 0, len(fieldSet))
	for k := range fieldSet {
		fields = append(fields, k)
	}
	sort.Strings(fields)

	var sharedRemain int64
	if shared != nil && shared.Available && !shared.Unlimited && shared.Remain > 0 {
		sharedRemain = int64(shared.Remain)
	}

	bd := &QuotaBreakdown{
		Remain:          int64(sumRemain),
		RemainEffective: int64(sumRemain) + sharedRemain,
		Used:            int64(sumUsed),
		Size:            int64(sumSize),
		Shared:          shared,
		Packages:        pkgs,
		Composition:     found,
		Fields:          fields,
	}
	if top, ok := payload.(map[string]any); ok {
		if ep, ok := firstEpoch(top, "expiresAt", "expireAt", "expires_at"); ok {
			bd.ExpiresAt = ep
		}
		if pct, ok := toFloat(top["totalUsagePercentage"]); ok {
			bd.UsagePct = pct
		}
		if exc, ok := top["isQuotaExceeded"].(bool); ok {
			bd.Exceeded = exc
		}
	}
	return bd, true
}

func computeExpiringAmount(bd *QuotaBreakdown, window time.Duration) float64 {
	if bd == nil || window <= 0 {
		return 0
	}
	cutoff := time.Now().Add(window).Unix()
	var sum float64
	for _, b := range bd.Packages {
		if !b.Available || b.Remain <= 0 || b.NoExpiry {
			continue
		}
		if b.ExpiresAt > 0 && b.ExpiresAt <= cutoff {
			sum += b.Remain
		} else if b.ExpiresAt == 0 && b.DaysLeft > 0 && float64(b.DaysLeft) <= window.Hours()/24.0 {
			sum += b.Remain
		}
	}
	if sum == 0 && bd.ExpiresAt > 0 && bd.ExpiresAt <= cutoff && bd.Remain > 0 {
		sum = float64(bd.Remain)
	}
	return sum
}

// FetchCredits fetches account quota breakdown using cached endpoint preference or probing.
func (c *Client) FetchCredits(ctx context.Context, a *auth.Auth) (*QuotaBreakdown, error) {
	if a == nil {
		return nil, fmt.Errorf("nil auth")
	}
	tok := a.AccessTokenValue()
	if tok == "" {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "missing access token"}
	}
	cfg := c.RealmConfigForAccount(a)
	scope := cfg.Realm
	if cfg.VPCInstance != "" {
		scope = cfg.Realm + ":vpc:" + cfg.VPCInstance
	}
	sources := c.loadCreditsSources()
	if known, ok := sources[scope]; ok && known.Host != "" && known.Path != "" {
		bd, st, err := c.fetchCreditsOnce(ctx, a, known.Host, known.Path)
		if err == nil && bd != nil {
			return bd, nil
		}
		if st == http.StatusUnauthorized {
			return nil, err
		}
	}

	// Default CN/primary endpoint first.
	bd, st, err := c.fetchCreditsOnce(ctx, a, cfg.OpenAPI, DefaultCreditsPath)
	if err == nil && bd != nil {
		c.saveCreditsSource(scope, cfg.OpenAPI, DefaultCreditsPath, bd.Fields)
		return bd, nil
	}
	if st == http.StatusUnauthorized {
		return nil, err
	}

	// Probe fallbacks (especially helpful for Global quota endpoint changes).
	res, perr := c.ProbeCreditsEndpoints(ctx, a, 6)
	if perr == nil && res != nil && res.OK && res.Hit != nil {
		return res.Hit, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no recognizable quota fields returned from %s", cfg.Realm)
}

func (c *Client) fetchCreditsOnce(ctx context.Context, a *auth.Auth, host, path string) (*QuotaBreakdown, int, error) {
	fullURL := strings.TrimRight(host, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, 0, err
	}
	cfg := c.RealmConfigForAccount(a)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ue := ClassifyError(resp.StatusCode, string(raw))
		return nil, resp.StatusCode, ue
	}
	var payload any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, resp.StatusCode, fmt.Errorf("invalid quota JSON: %s", string(raw[:minInt(len(raw), 120)]))
	}
	bd, ok := NormalizeQuotaPayload(payload, cfg.Realm)
	if !ok {
		return nil, resp.StatusCode, fmt.Errorf("unrecognized quota structure")
	}
	bd.SourceHost = host
	bd.SourcePath = path
	return bd, resp.StatusCode, nil
}

// ProbeCreditsEndpoints tests candidate host+path combinations and persists the working endpoint.
func (c *Client) ProbeCreditsEndpoints(ctx context.Context, a *auth.Auth, limit int) (*CreditsProbeResult, error) {
	if a == nil {
		return nil, fmt.Errorf("nil auth")
	}
	cfg := c.RealmConfigForAccount(a)
	var hosts []string
	for _, h := range []string{cfg.OpenAPI, cfg.Website} {
		h = strings.TrimRight(strings.TrimSpace(h), "/")
		if h != "" && !containsStr(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	type pair struct{ host, path string }
	var candidates []pair
	for _, h := range hosts {
		for _, p := range CreditsCandidatePaths {
			candidates = append(candidates, pair{host: h, path: p})
		}
	}
	if limit <= 0 {
		limit = 4
	}
	res := &CreditsProbeResult{Realm: cfg.Realm}
	scope := cfg.Realm
	if cfg.VPCInstance != "" {
		scope = cfg.Realm + ":vpc:" + cfg.VPCInstance
	}

	tried := 0
	for _, cand := range candidates {
		if tried >= limit {
			break
		}
		tried++
		fullURL := cand.host + cand.path
		row := ProbeRow{URL: fullURL, Host: cand.host, Path: cand.path}
		bd, st, err := c.fetchCreditsOnce(ctx, a, cand.host, cand.path)
		row.Status = st
		if err != nil {
			row.Error = err.Error()
			res.Rows = append(res.Rows, row)
			if st == http.StatusUnauthorized {
				return res, err
			}
			continue
		}
		if bd != nil {
			row.OK = true
			row.Fields = bd.Fields
			res.OK = true
			if res.Hit == nil {
				res.Hit = bd
				c.saveCreditsSource(scope, cand.host, cand.path, bd.Fields)
			}
		} else {
			row.Note = "200 without quota fields"
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// UserResourceDetailed returns (effectiveRemainCredits, detailedBreakdown, error),
// aligned with workbuddy2api's scheduler & expiring-window contract.
func (c *Client) UserResourceDetailed(a *auth.Auth, expiringWindow time.Duration) (float64, *ExpiringCreditsDetail, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bd, err := c.FetchCredits(ctx, a)
	if err != nil {
		return 0, &ExpiringCreditsDetail{Degraded: true}, err
	}
	expAmt := computeExpiringAmount(bd, expiringWindow)
	bd.ExpiringSoon = int64(expAmt)
	detail := &ExpiringCreditsDetail{
		Total:       float64(bd.RemainEffective),
		Expiring:    expAmt,
		SoonWindow:  expiringWindow,
		Buckets:     bd.Packages,
		SharedPool:  bd.Shared,
		Degraded:    bd.Degraded,
		SourcePath:  bd.SourceHost + bd.SourcePath,
		Composition: bd.Composition,
	}
	return float64(bd.RemainEffective), detail, nil
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// ParseFlexibleCredits parses a numeric value from UI/config or JSON fields cleanly.
func ParseFlexibleCredits(v any) float64 {
	if f, ok := toFloat(v); ok {
		return f
	}
	if s, ok := v.(string); ok {
		f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f
	}
	return 0
}
