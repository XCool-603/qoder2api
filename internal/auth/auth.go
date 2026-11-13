// Package auth parses Qoder credential files (nested/flat/qoder2api-hub/PAT/raw formats)
// for both CN (qoder.com.cn) and Global (qoder.com / qoder.sh) realms, and saves updates atomically.
package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qoder2api/internal/logfmt"
)

// CheckinFingerprint holds optional real Qoder Desktop device fingerprint headers
// used to unlock desktop-only models (smodel/cmodel/fast) or Global check-in campaigns.
type CheckinFingerprint struct {
	MachineID    string `json:"machine_id,omitempty"`
	MachineToken string `json:"machine_token,omitempty"`
	MachineType  string `json:"machine_type,omitempty"`
	MachineCode  string `json:"machine_code,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	MachineOS    string `json:"machine_os,omitempty"`
	Version      string `json:"version,omitempty"`
	ClientType   string `json:"client_type,omitempty"`
	Scene        string `json:"scene,omitempty"`
	Product      string `json:"product,omitempty"`
	DataPolicy   string `json:"data_policy,omitempty"`
	ClientIP     string `json:"client_ip,omitempty"`
}

// Auth is the normalized credential record for one Qoder account.
type Auth struct {
	mu sync.Mutex

	AccessToken   string
	RefreshToken  string
	PersonalToken string // pt- long-lived PAT (fallback for jobToken exchange)
	ExpiresAt     int64  // Unix seconds
	Domain        string
	realm         string // "cn" | "global"
	UID           string
	EnterpriseID  string
	Nickname      string
	FilePath      string

	// Qoder-specific account metadata
	VPCEndpoint        string              // Optional CN enterprise VPC instance/domain
	Platform           string              // "CLI" | "App"
	UserType           string              // e.g. "personal_professional_trial", "personal_standard"
	OrganizationID     string
	OrganizationName   string
	DeviceToken        string
	CheckinFingerprint *CheckinFingerprint
	CampaignCodes      map[string]string
	LastCheckinAt      int64

	Owner    string
	provider string // "oauth" | "pat" | "desktop-app" | "cli" | "import" | "google" | ...
}

// NormalizeProvider normalizes identity/source provider labels.
func NormalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "google", "google.com", "gmail":
		return "google"
	case "twitter", "twitter.com", "x", "x.com":
		return "twitter"
	case "github", "github.com":
		return "github"
	case "wechat", "weixin", "wx":
		return "wechat"
	case "qq":
		return "qq"
	case "desktop", "desktop-app", "app":
		return "desktop-app"
	}
	if strings.Contains(p, "google") {
		return "google"
	}
	if strings.Contains(p, "twitter") || p == "x" {
		return "twitter"
	}
	if strings.Contains(p, "github") {
		return "github"
	}
	return p
}

func (a *Auth) ProviderValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

func (a *Auth) Provider() string { return a.ProviderValue() }

func (a *Auth) SetProvider(provider string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = NormalizeProvider(provider)
}

func (a *Auth) OwnerValue() string {
	if a == nil {
		return "public"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.Owner) == "" {
		return "public"
	}
	return a.Owner
}

func (a *Auth) SetOwner(owner string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Owner = strings.TrimSpace(owner)
}

func (a *Auth) Lock()   { a.mu.Lock() }
func (a *Auth) Unlock() { a.mu.Unlock() }

func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

func (a *Auth) PersonalTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.PersonalToken
}

func (a *Auth) UserTypeValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.UserType
}

func (a *Auth) VPCEndpointValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.VPCEndpoint
}

var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled controls whether global realm routing is allowed (false forces pure CN mode).
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm returns normalized realm ("cn" or "global").
func (a *Auth) Realm() string {
	if a == nil {
		return "cn"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	r := strings.ToLower(strings.TrimSpace(a.realm))
	if r == "global" || r == "intl" || r == "gl" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm normalizes realm from explicit hint or domain.
func ResolveRealm(explicit, domain string) string {
	r := strings.ToLower(strings.TrimSpace(explicit))
	if r == "intl" || r == "gl" || r == "global" {
		return "global"
	}
	if r == "cn" {
		return "cn"
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		norm := ResolveRealm(a.realm, a.Domain)
		if norm != a.realm {
			a.realm = norm
			return true, norm
		}
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "qoder.com" || strings.HasSuffix(d, ".qoder.com") ||
		d == "qoder.sh" || strings.HasSuffix(d, ".qoder.sh") ||
		d == "qoder.ai" || strings.HasSuffix(d, ".qoder.ai")
}

// NeedsRefresh reports whether token expires within the given window (or lacks a non-placeholder AT).
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" && strings.TrimSpace(a.PersonalToken) == "" {
		return false
	}
	if a.AccessToken == "" || a.AccessToken == "pending_refresh" || a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// CleanRawInput strips UTF-8 BOM, markdown code fences, curly quotes, and CRLF.
func CleanRawInput(raw []byte) []byte {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte("\r"), []byte("\n"))
	raw = bytes.TrimSpace(raw)
	if bytes.HasPrefix(raw, []byte("```")) {
		if idx := bytes.IndexByte(raw, '\n'); idx != -1 {
			raw = raw[idx+1:]
		}
		if idx := bytes.LastIndex(raw, []byte("```")); idx != -1 {
			raw = raw[:idx]
		}
		raw = bytes.TrimSpace(raw)
	}
	raw = bytes.ReplaceAll(raw, []byte("“"), []byte("\""))
	raw = bytes.ReplaceAll(raw, []byte("”"), []byte("\""))
	raw = bytes.ReplaceAll(raw, []byte("‘"), []byte("'"))
	raw = bytes.ReplaceAll(raw, []byte("’"), []byte("'"))
	return raw
}

// SanitizeFilename removes path traversal and OS-invalid characters from filenames.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "account"
	}
	name = filepath.Base(name)
	name = strings.TrimPrefix(name, "..")
	var sb strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\r', '\n', '\t', 0:
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	res := strings.TrimSpace(sb.String())
	res = strings.Trim(res, "._ ")
	if res == "" {
		return "account"
	}
	return res
}

func (a *Auth) SetRealm(realm string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.realm = ResolveRealm(realm, a.Domain)
}

func (a *Auth) RawUID() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rawUIDLocked()
}

func (a *Auth) RawUIDLocked() string {
	if a == nil {
		return ""
	}
	return a.rawUIDLocked()
}

// CleanRawUID strips realm/provider prefixes and disambiguation suffixes.
func CleanRawUID(uid string) string {
	if uid == "" {
		return ""
	}
	for _, p := range []string{"cn-", "global-", "intl-", "cn:", "global:", "intl:"} {
		if strings.HasPrefix(strings.ToLower(uid), p) {
			uid = uid[len(p):]
			break
		}
	}
	for _, p := range []string{"google-", "twitter-", "github-", "oauth-", "pat-", "desktop-app-", "cli-", "personal-", "wechat-", "qq-", "phone-", "email-"} {
		if strings.HasPrefix(strings.ToLower(uid), p) {
			uid = uid[len(p):]
			break
		}
	}
	if idx := strings.IndexByte(uid, '#'); idx != -1 {
		uid = uid[:idx]
	}
	return uid
}

func (a *Auth) rawUIDLocked() string {
	uid := a.UID
	if uid == "" {
		return ""
	}
	realm := a.realmLocked()
	if strings.HasPrefix(uid, realm+"-") {
		uid = strings.TrimPrefix(uid, realm+"-")
	} else if strings.HasPrefix(uid, realm+":") {
		uid = strings.TrimPrefix(uid, realm+":")
	}
	return CleanRawUID(uid)
}

// ParseJWTClaims decodes JWT payload claims when present.
func ParseJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[1]
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	_ = json.Unmarshal(b, &claims)
	return claims
}

func tryParseRawToken(str string) *Auth {
	str = strings.TrimSpace(str)
	if str == "" {
		return nil
	}
	lowStr := strings.ToLower(str)
	if strings.HasPrefix(lowStr, "http://") || strings.HasPrefix(lowStr, "https://") {
		return nil
	}
	str = strings.TrimPrefix(str, "Bearer ")
	str = strings.TrimPrefix(str, "bearer ")

	// Reject raw web URLs (e.g. user accidentally pasting the OAuth login URL into token input)
	low := strings.ToLower(str)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil
	}

	var at, rt, pt, explicitRealm, explicitUID, explicitNick, explicitProvider string

	if strings.Contains(str, "\n") || strings.Contains(str, "=") || (strings.Contains(str, ":") && !strings.HasPrefix(str, "http")) {
		lines := strings.Split(str, "\n")
		var kvAt, kvRt, kvPt string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			var k, v string
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				k = strings.ToLower(strings.TrimSpace(parts[0]))
				v = strings.TrimSpace(parts[1])
			} else if strings.Contains(line, ":") {
				parts := strings.SplitN(line, ":", 2)
				k = strings.ToLower(strings.TrimSpace(parts[0]))
				v = strings.TrimSpace(parts[1])
			}
			v = strings.Trim(v, `"'`)
			switch k {
			case "access_token", "accesstoken", "token", "device_token", "jwt", "apikey":
				kvAt = v
			case "refresh_token", "refreshtoken", "refresh":
				kvRt = v
			case "personal_token", "personaltoken", "pat":
				kvPt = v
			case "realm", "region":
				explicitRealm = v
			case "uid", "user_id", "userid", "id":
				explicitUID = v
			case "nickname", "nick", "name":
				explicitNick = v
			case "provider", "idp", "identity_provider", "source", "type":
				explicitProvider = v
			}
		}
		if kvAt != "" {
			at = kvAt
		}
		if kvRt != "" {
			rt = kvRt
		}
		if kvPt != "" {
			pt = kvPt
		}
	}

	if at == "" && pt == "" {
		for _, sep := range []string{"----", "---", "|", "\t", "#", ","} {
			if strings.Contains(str, sep) {
				parts := strings.SplitN(str, sep, 2)
				p0 := strings.TrimSpace(parts[0])
				p1 := strings.TrimSpace(parts[1])
				if p0 != "" && p1 != "" {
					if strings.HasPrefix(p0, "pt-") {
						pt = p0
						rt = p1
					} else {
						at = p0
						rt = p1
					}
					break
				}
			}
		}
	}

	if at == "" && pt == "" {
		if strings.HasPrefix(str, "pt-") {
			pt = str
		} else {
			at = str
		}
	}
	if pt == "" && strings.HasPrefix(at, "pt-") {
		pt = at
		at = "pending_refresh"
	}
	if strings.HasPrefix(strings.ToLower(at), "http://") || strings.HasPrefix(strings.ToLower(at), "https://") {
		return nil
	}
	if at == "" && pt != "" {
		at = "pending_refresh"
	}

	if len(at) < 10 && len(pt) < 10 {
		return nil
	}

	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	if at == "pending_refresh" {
		exp = 1
	}

	a := &Auth{
		AccessToken:   at,
		RefreshToken:  rt,
		PersonalToken: pt,
		ExpiresAt:     exp,
		realm:         ResolveRealm(explicitRealm, ""),
		UID:           explicitUID,
		Nickname:      explicitNick,
		provider:      NormalizeProvider(explicitProvider),
	}
	if explicitRealm == "" && pt != "" && a.provider == "" {
		a.provider = "pat"
	}

	if claims := ParseJWTClaims(at); claims != nil {
		if a.UID == "" {
			a.UID = extractValString(claims, "sub", "uid", "id", "user_id", "userId", "account_id", "email", "preferred_username")
		}
		if expClaim, ok := claims["exp"].(float64); ok && expClaim > 0 {
			a.ExpiresAt = int64(expClaim)
		}
		if a.Nickname == "" {
			a.Nickname = extractValString(claims, "name", "nickname", "nick_name", "username")
		}
		if a.provider == "" {
			a.provider = NormalizeProvider(extractValString(claims, "identity_provider", "idp", "auth_provider", "provider"))
		}
		if iss, ok := claims["iss"].(string); ok && iss != "" {
			if isGlobalDomain(iss) {
				a.Domain = iss
				if a.realm == "" {
					a.realm = "global"
				}
			}
		}
	}

	if a.UID == "" {
		a.UID = fmt.Sprintf("token_%d", time.Now().UnixNano()%100000000)
	}
	return a
}

func extractValString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if s := strings.TrimSpace(val); s != "" {
					return s
				}
			case json.Number:
				return val.String()
			case float64:
				return fmt.Sprintf("%.0f", val)
			case int:
				return strconv.Itoa(val)
			case int64:
				return strconv.FormatInt(val, 10)
			}
		}
	}
	return ""
}

func extractValInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case json.Number:
				if n, err := val.Int64(); err == nil {
					return n
				}
				if f, err := val.Float64(); err == nil {
					return int64(f)
				}
			case float64:
				return int64(val)
			case int64:
				return val
			case int:
				return int64(val)
			case string:
				s := strings.TrimSpace(val)
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return n
				}
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					return t.Unix()
				}
			}
		}
	}
	return 0
}

// Parse accepts nested {"auth","account"}, flat qoder2api-hub JSON, zhengyuuuui secret JSON,
// raw token/PAT strings, and token----refresh combinations.
func Parse(raw []byte) (*Auth, error) {
	raw = CleanRawInput(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}

	if !bytes.HasPrefix(raw, []byte("{")) {
		if a := tryParseRawToken(string(raw)); a != nil {
			return a, nil
		}
		return nil, fmt.Errorf("storage_parse_error: invalid character '%c' looking for beginning of JSON object", raw[0])
	}

	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}

	var authMap, accountMap, dataMap map[string]any
	if sub, ok := m["auth"].(map[string]any); ok {
		authMap = sub
	}
	if sub, ok := m["account"].(map[string]any); ok {
		accountMap = sub
	}
	for _, k := range []string{"data", "result", "response", "token_info", "credentials"} {
		if sub, ok := m[k].(map[string]any); ok {
			dataMap = sub
			break
		}
	}

	pickStr := func(keys ...string) string {
		for _, layer := range []map[string]any{authMap, accountMap, dataMap, m} {
			if v := extractValString(layer, keys...); v != "" {
				return v
			}
		}
		return ""
	}

	at := pickStr("accessToken", "access_token", "token", "device_token", "deviceToken", "jwt", "bearer_token", "auth_token")
	rt := pickStr("refreshToken", "refresh_token", "refresh")
	pt := pickStr("personalToken", "personal_token", "pat")

	if pt == "" && strings.HasPrefix(at, "pt-") {
		pt = at
		at = "pending_refresh"
	}
	if strings.HasPrefix(strings.ToLower(at), "http://") || strings.HasPrefix(strings.ToLower(at), "https://") {
		return nil, fmt.Errorf("storage_parse_error: access token is an OAuth authorization URL, not a valid bearer token")
	}

	var expAt int64
	for _, layer := range []map[string]any{authMap, dataMap, m} {
		if v := extractValInt64(layer, "expiresAt", "expires_at", "expire_at", "expire_time", "exp"); v > 0 {
			if v > 1e12 {
				v /= 1000
			}
			expAt = v
			break
		}
	}
	if expAt == 0 {
		for _, layer := range []map[string]any{authMap, dataMap, m} {
			if expIn := extractValInt64(layer, "expiresIn", "expires_in", "expire_in"); expIn > 0 {
				if expIn > 1e8 {
					expIn /= 1000
				}
				expAt = time.Now().Unix() + expIn
				break
			}
		}
	}

	domain := pickStr("domain", "api_domain", "host")
	realmRaw := pickStr("realm", "region")
	realm := ""
	if realmRaw != "" {
		realm = ResolveRealm(realmRaw, domain)
	}

	uid := pickStr("uid", "user_id", "userId", "id", "sub", "account_id", "email", "preferred_username")
	ent := pickStr("enterpriseId", "enterprise_id", "enterpriseID")
	nick := pickStr("nickname", "nick_name", "name", "username", "email")
	deviceToken := pickStr("device_token", "deviceToken")
	owner := extractValString(m, "owner")
	if owner == "" {
		owner = extractValString(accountMap, "owner")
	}

	vpc := pickStr("vpcEndpoint", "vpc_endpoint")
	platform := pickStr("platform")
	userType := pickStr("userType", "user_type")
	orgID := pickStr("organizationId", "organization_id")
	orgName := pickStr("organizationName", "organization_name")

	if strings.TrimSpace(at) == "" && (strings.TrimSpace(rt) != "" || strings.TrimSpace(pt) != "") {
		at = "pending_refresh"
		if expAt == 0 {
			expAt = 1
		}
	}
	if strings.TrimSpace(at) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}

	provider := pickStr("provider", "idp", "identity_provider", "auth_provider", "login_provider", "login_type", "source", "type")

	if claims := ParseJWTClaims(at); claims != nil {
		if expAt == 0 {
			if exp, ok := claims["exp"].(float64); ok && exp > 0 {
				expAt = int64(exp)
			}
		}
		if uid == "" {
			uid = extractValString(claims, "sub", "uid", "id", "user_id", "userId", "account_id", "email", "preferred_username")
		}
		if nick == "" {
			nick = extractValString(claims, "name", "nickname", "nick_name", "username")
		}
		if provider == "" {
			provider = extractValString(claims, "identity_provider", "idp", "federated_identity_provider", "auth_provider", "provider")
		}
	}

	var fp *CheckinFingerprint
	for _, key := range []string{"checkin_fingerprint", "checkinFingerprint", "fingerprint"} {
		if rawFP, ok := m[key]; ok && rawFP != nil {
			if b, err := json.Marshal(rawFP); err == nil {
				var parsedFP CheckinFingerprint
				if json.Unmarshal(b, &parsedFP) == nil && (parsedFP.MachineToken != "" || parsedFP.MachineID != "") {
					fp = &parsedFP
					break
				}
			}
		}
	}

	var codes map[string]string
	if rawCodes, ok := m["campaignCodes"].(map[string]any); ok && len(rawCodes) > 0 {
		codes = make(map[string]string, len(rawCodes))
		for k, v := range rawCodes {
			if s, ok := v.(string); ok && s != "" {
				codes[k] = s
			}
		}
	}

	a := &Auth{
		AccessToken:        at,
		RefreshToken:       rt,
		PersonalToken:      pt,
		ExpiresAt:          expAt,
		Domain:             domain,
		realm:              realm,
		UID:                uid,
		EnterpriseID:       ent,
		Nickname:           nick,
		VPCEndpoint:        vpc,
		Platform:           platform,
		UserType:           userType,
		OrganizationID:     orgID,
		OrganizationName:   orgName,
		DeviceToken:        deviceToken,
		CheckinFingerprint: fp,
		CampaignCodes:      codes,
		Owner:              owner,
		provider:           NormalizeProvider(provider),
	}
	return a, nil
}

// SaveAtomic atomically writes the normalized Qoder auth record to FilePath.
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.RefreshToken) == "" && strings.TrimSpace(a.PersonalToken) == "" {
		return fmt.Errorf("save refused: empty credentials (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	authObj := doc["auth"].(map[string]any)
	acctObj := doc["account"].(map[string]any)
	if a.PersonalToken != "" {
		authObj["personalToken"] = a.PersonalToken
		doc["personalToken"] = a.PersonalToken
	}
	if a.VPCEndpoint != "" {
		doc["vpcEndpoint"] = a.VPCEndpoint
	}
	if a.Platform != "" {
		doc["platform"] = a.Platform
	}
	if a.UserType != "" {
		doc["userType"] = a.UserType
	}
	if a.OrganizationID != "" {
		acctObj["organizationId"] = a.OrganizationID
	}
	if a.OrganizationName != "" {
		acctObj["organizationName"] = a.OrganizationName
	}
	if a.provider != "" {
		acctObj["provider"] = a.provider
		authObj["provider"] = a.provider
	}
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	if a.CheckinFingerprint != nil {
		doc["checkin_fingerprint"] = a.CheckinFingerprint
	}
	if len(a.CampaignCodes) > 0 {
		doc["campaignCodes"] = a.CampaignCodes
	}
	if a.Owner != "" {
		doc["owner"] = a.Owner
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		if writeErr := os.WriteFile(a.FilePath, raw, 0o644); writeErr != nil {
			return fmt.Errorf("write tmp failed: %w (fallback direct write: %v)", err, writeErr)
		}
		return nil
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, a.FilePath); err != nil {
		_ = os.Remove(a.FilePath)
		if err2 := os.Rename(tmp, a.FilePath); err2 != nil {
			if writeErr := os.WriteFile(a.FilePath, raw, 0o644); writeErr != nil {
				return fmt.Errorf("rename failed (%v) and fallback direct write failed: %w", err, writeErr)
			}
		}
	}
	return nil
}

// AuthFileGlob is the canonical credential file pattern inside ./auths.
const AuthFileGlob = "qoder*.json"

// LoadAuthFiles returns sorted credential file paths inside dir.
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[filepath.Clean(f)] = true
	}

	allJSON, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range allJSON {
		clean := filepath.Clean(f)
		if seen[clean] {
			continue
		}
		base := strings.ToLower(filepath.Base(clean))
		if base == "state.json" || base == "users.json" || base == "settings.json" ||
			base == "config.json" || base == "metrics.json" || base == "credits-source.json" ||
			base == "model.json" {
			continue
		}
		if raw, err := os.ReadFile(clean); err == nil && len(raw) > 0 {
			if a, parseErr := Parse(raw); parseErr == nil && a != nil && (a.AccessToken != "" || a.PersonalToken != "") {
				files = append(files, f)
				seen[clean] = true
			}
		}
	}

	sort.Strings(files)
	return files, nil
}

// LoadDir scans dir, parses credentials, normalizes realms, and backfills missing realm metadata.
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if a.UID == "" {
			base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
			base = strings.TrimPrefix(base, "qoder-")
			base = strings.TrimPrefix(base, "qoder_")
			if base != "" {
				a.UID = base
			}
		}
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s", logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		if a.Owner == "" {
			a.Owner = "public"
		}
		if a.provider == "" {
			base := strings.ToLower(filepath.Base(f))
			switch {
			case strings.Contains(base, "twitter") || strings.Contains(base, "-x-"):
				a.provider = "twitter"
			case strings.Contains(base, "google"):
				a.provider = "google"
			case strings.Contains(base, "github"):
				a.provider = "github"
			}
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" || a.RealmStored() == "intl" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("[auth] %s migrated realm=global (domain=%s)", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
