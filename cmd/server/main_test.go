package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"qoder2api/internal/auth"
	"qoder2api/internal/pool"
	"qoder2api/internal/redisstore"
	"qoder2api/internal/server"
)

func realmTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	cn := &auth.Auth{UID: "cn1", Domain: "qoder.com.cn"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "gl1", Domain: "qoder.com"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)
	return p
}

func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmTestPool(t)
	fn := realmAwareAvailableForModel(p)

	cases := []struct {
		model string
		want  []string
	}{
		{"q37fmodel", []string{"cn1"}},
		{"cn:glm-5.2", []string{"cn1"}},
		{"global:smodel", []string{"gl1"}},
		{"smodel", []string{"gl1"}},
	}
	for _, tc := range cases {
		if got := fn(tc.model); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("AvailableForModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := pool.New("")
	gl := &auth.Auth{UID: "gl1", Domain: "qoder.com"}
	gl.SetRealm("global")
	p.Add(gl)

	fn := realmAwareAvailableForModel(p)
	if got := fn("global:smodel"); len(got) != 0 {
		t.Errorf("global disabled: want empty for global:smodel, got %v", got)
	}
	if got := fn("cn:glm-5.2"); !reflect.DeepEqual(got, []string{"gl1"}) {
		t.Errorf("global disabled: want gl1 mapped to cn, got %v", got)
	}
}

func TestConfigLoadDefaultsSettingsAndEnv(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.json")
	stateFile := filepath.Join(tmp, "data", "state.json")
	_ = os.MkdirAll(filepath.Dir(stateFile), 0755)
	_ = os.WriteFile(cfgFile, []byte(`{
		"listen": ":7865",
		"state_file": "`+filepath.ToSlash(stateFile)+`",
		"admin": {"enabled": true},
		"api_key": "initial-key"
	}`), 0644)
	_ = os.WriteFile(filepath.Join(filepath.Dir(stateFile), "settings.json"), []byte(`{
		"api_key": "persisted-key",
		"ip_whitelist_enabled": true,
		"allowed_ips": ["10.0.0.0/8"]
	}`), 0600)

	t.Setenv("QD2A_LISTEN", "18080")
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Listen != ":18080" {
		t.Fatalf("expected :18080 from env, got %q", cfg.Listen)
	}
	if cfg.APIKey != "persisted-key" {
		t.Fatalf("expected persisted-key from settings.json, got %q", cfg.APIKey)
	}
	if !cfg.IPWhitelistEnabled || len(cfg.AllowedIPs) != 1 || cfg.AllowedIPs[0] != "10.0.0.0/8" {
		t.Fatalf("expected persisted IP whitelist, got enabled=%v ips=%v", cfg.IPWhitelistEnabled, cfg.AllowedIPs)
	}
}

func TestConfigAdminEnabledRequiresAPIKey(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "bad.json")
	_ = os.WriteFile(cfgFile, []byte(`{"admin":{"enabled":true},"api_key":""}`), 0644)
	if _, err := Load(cfgFile); err == nil {
		t.Fatal("expected error when admin.enabled=true and api_key is empty")
	}
}

func TestBuildWiringSmokeHealthz(t *testing.T) {
	cfg := Default()
	cfg.AuthDir = t.TempDir()
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	p := buildPool(cfg, redisstore.Noop{}, nil)
	defer p.Close()
	up := buildUpstream(cfg)

	h, err := server.New(server.Config{
		Pool:          p,
		Upstream:      up,
		APIKey:        "sk-smoke",
		GlobalEnabled: cfg.Global.Enabled,
	})
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty pool GET /healthz = %d, want 503", rec.Code)
	}

	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "qoder.com"})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("servable pool GET /healthz = %d, want 200", rec2.Code)
	}
}
