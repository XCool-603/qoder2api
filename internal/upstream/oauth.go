package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/cosy"
)

// OAuthStartResult contains the PKCE challenge state and authorization URL for browser login.
type OAuthStartResult struct {
	State       string `json:"state"`
	AuthURL     string `json:"auth_url"`
	Realm       string `json:"realm"`
	Platform    string `json:"platform,omitempty"`
	VPCEndpoint string `json:"vpc_endpoint,omitempty"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
}

// UserInfo represents the response from Qoder /api/v1/userinfo.
type UserInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Nickname         string `json:"nickname"`
	Email            string `json:"email"`
	UserType         string `json:"user_type"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
}

// GeneratePKCE builds an RFC 7636 S256 verifier + challenge pair.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// StartOAuth builds the CN or Global PKCE device authorization URL.
func (c *Client) StartOAuth(realm, platform, vpcEndpoint, machineUID string) (*OAuthStartResult, error) {
	r := NormalizeRealm(realm)
	cfg := DefaultRealmConfig(r)
	if r == RealmCN {
		vpc := vpcEndpoint
		if vpc == "" && c != nil {
			vpc = c.GlobalVPC
		}
		if vpc != "" {
			if inst, web, oapi, gw, err := ParseVPCEndpoint(vpc); err != nil {
				return nil, err
			} else if inst != "" {
				cfg.VPCInstance = inst
				cfg.Website = web
				cfg.OpenAPI = oapi
				cfg.Gateways = []string{gw}
			}
		}
	}
	if c != nil {
		if r == RealmCN && c.WebsiteCN != "" {
			cfg.Website = strings.TrimRight(c.WebsiteCN, "/")
		} else if r == RealmGlobal && c.WebsiteGlobal != "" {
			cfg.Website = strings.TrimRight(c.WebsiteGlobal, "/")
		}
	}

	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	nonce := cosy.NewSimpleID()
	if cfg.NonceDashed {
		nonce = cosy.NewUUID()
	}

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	if cfg.SendRedirectURI && cfg.RedirectURI != "" {
		q.Set("redirect_uri", cfg.RedirectURI)
	}
	if cfg.SendClientID && cfg.ClientID != "" {
		q.Set("client_id", cfg.ClientID)
		seed := cosy.FingerprintSeed(machineUID, nonce)
		q.Set("machine_id", cosy.DeriveMachineID(seed))
	}
	authURL := strings.TrimRight(cfg.Website, "/") + PathDeviceSelect + "?" + q.Encode()
	state := fmt.Sprintf("qd-%d", time.Now().UnixNano())
	return &OAuthStartResult{
		State:       state,
		AuthURL:     authURL,
		Realm:       r,
		Platform:    platform,
		VPCEndpoint: cfg.VPCInstance,
		Nonce:       nonce,
		Verifier:    verifier,
	}, nil
}

// PollOAuthOnce polls /api/v1/deviceToken/poll once.
// Returns (nil, true, nil) while waiting for browser consent (404/202).
func (c *Client) PollOAuthOnce(ctx context.Context, realm, vpcEndpoint, nonce, verifier string) (acct *auth.Auth, pending bool, err error) {
	r := NormalizeRealm(realm)
	tmpAuth := &auth.Auth{VPCEndpoint: vpcEndpoint}
	tmpAuth.SetRealm(r)
	cfg := c.RealmConfigForAccount(tmpAuth)

	q := url.Values{}
	q.Set("nonce", nonce)
	q.Set("verifier", verifier)
	q.Set("challenge_method", "S256")
	pollURL := strings.TrimRight(cfg.OpenAPI, "/") + PathDevicePoll + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, false, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, ClassifyError(resp.StatusCode, string(raw))
	}

	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return nil, false, fmt.Errorf("invalid poll response JSON: %s", string(raw[:minInt(len(raw), 160)]))
	}
	tok := firstNonEmptyStr(data, "token", "device_token", "access_token")
	ref := firstNonEmptyStr(data, "refresh_token", "refreshToken")
	if tok == "" {
		// Some upstream implementations wrap payload inside "data" object.
		if nested, ok := data["data"].(map[string]any); ok {
			tok = firstNonEmptyStr(nested, "token", "device_token", "access_token")
			if ref == "" {
				ref = firstNonEmptyStr(nested, "refresh_token", "refreshToken")
			}
			data = nested
		}
	}
	if tok == "" {
		return nil, true, nil
	}

	exp := parseQoderTokenExpiry(data, 30*24*time.Hour)
	a := &auth.Auth{
		AccessToken:  tok,
		RefreshToken: ref,
		ExpiresAt:    exp,
		Domain:       domainForRealm(r),
		VPCEndpoint:  vpcEndpoint,
	}
	a.SetRealm(r)
	if ui, uerr := c.FetchUserInfo(ctx, a); uerr == nil && ui != nil {
		if ui.ID != "" {
			a.UID = ui.ID
		}
		if ui.Nickname != "" {
			a.Nickname = ui.Nickname
		} else if ui.Name != "" {
			a.Nickname = ui.Name
		} else if ui.Email != "" {
			a.Nickname = ui.Email
		}
		if ui.UserType != "" {
			a.UserType = ui.UserType
		}
		a.OrganizationID = ui.OrganizationID
		a.OrganizationName = ui.OrganizationName
	}
	if a.UID == "" {
		claims := auth.ParseJWTClaims(tok)
		if uid := firstNonEmptyStr(claims, "uid", "sub", "user_id", "id"); uid != "" {
			a.UID = uid
		}
	}
	if a.UID == "" {
		a.UID = "qd-" + cosy.NewSimpleID()[:12]
	}
	return a, false, nil
}

// FetchUserInfo queries /api/v1/userinfo using Bearer access token.
func (c *Client) FetchUserInfo(ctx context.Context, a *auth.Auth) (*UserInfo, error) {
	if a == nil || a.AccessTokenValue() == "" {
		return nil, fmt.Errorf("missing token")
	}
	cfg := c.RealmConfigForAccount(a)
	u := strings.TrimRight(cfg.OpenAPI, "/") + PathUserInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ClassifyError(resp.StatusCode, string(raw))
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, fmt.Errorf("invalid userinfo payload")
	}
	if sub, ok := m["data"].(map[string]any); ok && sub["id"] != nil {
		m = sub
	}
	ui := &UserInfo{
		ID:               firstNonEmptyStr(m, "id", "uid", "user_id", "userId"),
		Name:             firstNonEmptyStr(m, "name", "username"),
		Nickname:         firstNonEmptyStr(m, "nickname", "nickName", "name"),
		Email:            firstNonEmptyStr(m, "email"),
		UserType:         firstNonEmptyStr(m, "user_type", "userType"),
		OrganizationID:   firstNonEmptyStr(m, "organization_id", "organizationId"),
		OrganizationName: firstNonEmptyStr(m, "organization_name", "organizationName"),
	}
	return ui, nil
}

// ExchangePersonalToken trades a long-lived Qoder PAT (pt-...) for short-lived jt-/jrt- session tokens.
func (c *Client) ExchangePersonalToken(ctx context.Context, a *auth.Auth) error {
	if a == nil || a.PersonalTokenValue() == "" {
		return fmt.Errorf("missing personal token (pt-)")
	}
	pt := a.PersonalTokenValue()
	cfg := c.RealmConfigForAccount(a)
	bodyBytes, _ := json.Marshal(map[string]string{"personal_token": pt})
	err := c.postTokenExchange(ctx, strings.TrimRight(cfg.OpenAPI, "/")+PathJobExchange, bodyBytes, a, cfg.UserAgent, false)
	if err == nil {
		c.enrichFromUserInfoIfMissing(ctx, a)
		return nil
	}

	// Fallback to gateway encoded exchange (/algo/api/v3/user/jobToken?Encode=1).
	for _, gw := range c.OrderedGateways(a) {
		u := strings.TrimRight(gw, "/") + PathJobTokenV3
		ferr := c.postTokenExchange(ctx, u, bodyBytes, a, cfg.UserAgent, true)
		if ferr == nil {
			c.enrichFromUserInfoIfMissing(ctx, a)
			return nil
		}
	}
	return err
}

func (c *Client) enrichFromUserInfoIfMissing(ctx context.Context, a *auth.Auth) {
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	ui, err := c.FetchUserInfo(ctx, a)
	if err != nil || ui == nil {
		return
	}
	a.Lock()
	if ui.ID != "" && (a.UID == "" || strings.HasPrefix(a.UID, "qd-") || strings.HasPrefix(a.UID, "token_")) {
		a.UID = ui.ID
	}
	if a.Nickname == "" {
		if ui.Nickname != "" {
			a.Nickname = ui.Nickname
		} else if ui.Name != "" {
			a.Nickname = ui.Name
		} else if ui.Email != "" {
			a.Nickname = ui.Email
		}
	}
	if ui.UserType != "" {
		a.UserType = ui.UserType
	}
	if ui.OrganizationID != "" {
		a.OrganizationID = ui.OrganizationID
	}
	if ui.OrganizationName != "" {
		a.OrganizationName = ui.OrganizationName
	}
	a.Unlock()
	_ = a.SaveAtomic()
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(anyStr(m[k])); v != "" {
			return v
		}
	}
	return ""
}

func domainForRealm(realm string) string {
	if NormalizeRealm(realm) == RealmCN {
		return "qoder.com.cn"
	}
	return "qoder.com"
}

func parseQoderTokenExpiry(data map[string]any, defaultTTL time.Duration) int64 {
	if raw, ok := data["expires_in"]; ok && raw != nil {
		if f, fok := toFloat(raw); fok && f > 0 {
			// Qoder deviceToken/jobToken returns expires_in in milliseconds (or rarely seconds if < 1e6 and small).
			if f > 86400*120 { // clearly milliseconds (> 120 days if seconds, or typical 30d ms = 2.59e9)
				return time.Now().Unix() + int64(f/1000.0)
			}
			// Heuristic aligned with qoder2api-hub: values <= 1e7 wait: 30d in sec is 2.59e6; 30d in ms is 2.59e9; 1d in ms is 8.64e7.
			if f > 1e7 {
				return time.Now().Unix() + int64(f/1000.0)
			}
			return time.Now().Unix() + int64(f)
		}
	}
	if raw, ok := data["expires_at"]; ok && raw != nil {
		if ep, ok := parseEpochFlex(raw); ok && ep > time.Now().Unix() {
			return ep
		}
	}
	return time.Now().Add(defaultTTL).Unix()
}

// postTokenExchange is shared by PAT exchange and refresh flows.
func (c *Client) postTokenExchange(ctx context.Context, endpointURL string, jsonPayload []byte, a *auth.Auth, ua string, encoded bool) error {
	payloadBytes := jsonPayload
	contentType := "application/json"
	if encoded {
		enc, err := cosy.Encode(jsonPayload)
		if err != nil {
			return err
		}
		payloadBytes = []byte(enc)
		contentType = "text/plain"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	if a.PersonalTokenValue() != "" && encoded {
		req.Header.Set("Authorization", "Bearer "+a.PersonalTokenValue())
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		ue := ClassifyError(resp.StatusCode, string(raw))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			ue.Kind = ErrSessionDead
		}
		return ue
	}
	decoded := raw
	if encoded {
		if d, derr := cosy.Decode(strings.TrimSpace(string(raw))); derr == nil && len(d) > 0 {
			decoded = d
		}
	}
	var data map[string]any
	if json.Unmarshal(decoded, &data) != nil {
		return fmt.Errorf("token endpoint returned non-JSON: %s", string(decoded[:minInt(len(decoded), 160)]))
	}
	if nested, ok := data["data"].(map[string]any); ok && (nested["token"] != nil || nested["access_token"] != nil) {
		data = nested
	}
	tok := firstNonEmptyStr(data, "token", "device_token", "access_token")
	if tok == "" {
		return &Error{Kind: ErrSessionDead, Status: resp.StatusCode, Msg: "token endpoint returned empty token"}
	}
	ref := firstNonEmptyStr(data, "refresh_token", "refreshToken")
	exp := parseQoderTokenExpiry(data, 30*24*time.Hour)

	a.Lock()
	a.AccessToken = tok
	if ref != "" {
		a.RefreshToken = ref
	}
	if exp > 0 {
		a.ExpiresAt = exp
	}
	a.Unlock()

	_ = a.SaveAtomic()
	c.sessionCache().Invalidate(a.UID)
	return nil
}
