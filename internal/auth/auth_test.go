package auth

import (
	"path/filepath"
	"testing"
)

func TestParseQoderFormats(t *testing.T) {
	// 1. Nested format
	nested := []byte(`{"auth":{"accessToken":"dt-abc123456","refreshToken":"drt-xyz","domain":"qoder.com","realm":"intl"},"account":{"uid":"u100","nickname":"IntlUser"}}`)
	a1, err := Parse(nested)
	if err != nil {
		t.Fatalf("Parse nested failed: %v", err)
	}
	if a1.Realm() != "global" || a1.AccessToken != "dt-abc123456" || a1.RefreshToken != "drt-xyz" {
		t.Fatalf("unexpected nested parse: %+v", a1)
	}

	// 2. qoder2api-hub flat format
	flat := []byte(`{"uid":"u200","nickname":"CNUser","domain":"qoder.com.cn","realm":"cn","accessToken":"jt-1234567890","refreshToken":"jrt-999","personalToken":"pt-secret123456"}`)
	a2, err := Parse(flat)
	if err != nil {
		t.Fatalf("Parse flat failed: %v", err)
	}
	if a2.Realm() != "cn" || a2.PersonalToken != "pt-secret123456" || a2.UID != "u200" {
		t.Fatalf("unexpected flat parse: %+v", a2)
	}

	// 3. Raw PAT token
	a3, err := Parse([]byte("pt-my-personal-access-token-123456"))
	if err != nil {
		t.Fatalf("Parse raw PAT failed: %v", err)
	}
	if a3.PersonalToken != "pt-my-personal-access-token-123456" || !a3.NeedsRefresh(0) {
		t.Fatalf("expected PAT pending_refresh needing immediate exchange, got %+v", a3)
	}

	// 4. token----refreshToken
	a4, err := Parse([]byte("dt-token123456----drt-refresh123456"))
	if err != nil {
		t.Fatalf("Parse card line failed: %v", err)
	}
	if a4.AccessToken != "dt-token123456" || a4.RefreshToken != "drt-refresh123456" {
		t.Fatalf("unexpected card parse: %+v", a4)
	}
}

func TestSaveAtomicAndLoadDir(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "qoder-global-u1.json")
	a := &Auth{
		AccessToken:  "dt-save-token-123",
		RefreshToken: "drt-save-refresh-123",
		ExpiresAt:    9999999999,
		Domain:       "qoder.com",
		realm:        "global",
		UID:          "u1",
		Nickname:     "Alice",
		FilePath:     fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic failed: %v", err)
	}
	loaded, err := LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadDir failed: len=%d err=%v", len(loaded), err)
	}
	if loaded[0].UID != "u1" || loaded[0].Realm() != "global" {
		t.Fatalf("loaded mismatch: %+v", loaded[0])
	}
}
