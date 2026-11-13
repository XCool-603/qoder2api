package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"qoder2api/internal/auth"
)

// RefreshToken refreshes credentials routing by token prefix:
//   - drt-... -> POST {openapi}/api/v1/deviceToken/refresh
//   - jrt-... (or non-drt refresh token) -> POST {openapi}/api/v1/jobToken/refresh
//   - fallback -> PAT (pt-...) re-exchange via jobToken/exchange
// On success, updates account atomically and invalidates COSY SessionCache so next call re-signs.
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("nil auth")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.RefreshTokenContext(ctx, a)
}

// RefreshTokenContext performs credential refresh respecting caller context.
func (c *Client) RefreshTokenContext(ctx context.Context, a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("nil auth")
	}
	cfg := c.RealmConfigForAccount(a)
	base := strings.TrimRight(cfg.OpenAPI, "/")
	ref := a.RefreshTokenValue()
	pt := a.PersonalTokenValue()

	// 1) OAuth Device Token family (drt-...)
	if strings.HasPrefix(ref, "drt-") {
		payload, _ := json.Marshal(map[string]string{"refresh_token": ref})
		err := c.postTokenExchange(ctx, base+PathDeviceRefresh, payload, a, cfg.UserAgent, false)
		if err == nil {
			return nil
		}
		// Never silently overwrite OAuth session with PAT unless no OAuth path exists; surface error cleanly.
		return err
	}

	// 2) Job Token family (jrt-...) with PAT re-exchange fallback
	if ref != "" {
		payload, _ := json.Marshal(map[string]string{"refresh_token": ref})
		err := c.postTokenExchange(ctx, base+PathJobRefresh, payload, a, cfg.UserAgent, false)
		if err == nil {
			return nil
		}
		if pt == "" {
			return err
		}
	}

	// 3) Personal Access Token (pt-...) exchange
	if pt != "" {
		return c.ExchangePersonalToken(ctx, a)
	}

	return &Error{
		Kind:   ErrSessionDead,
		Status: 401,
		Msg:    "no refresh_token or personal_token available; please re-login",
	}
}
