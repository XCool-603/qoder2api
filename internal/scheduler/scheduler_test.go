package scheduler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"qoder2api/internal/auth"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
)

func TestNextFireCST(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, cstZone)
	fire := nextFire(now, []int{10, 22})
	want := time.Date(2026, 11, 12, 10, 0, 0, 0, cstZone)
	if !fire.Equal(want) {
		t.Fatalf("got %s want %s", fire, want)
	}
	fireLate := nextFire(time.Date(2026, 11, 12, 23, 0, 0, 0, cstZone), []int{10, 22})
	wantNext := time.Date(2026, 11, 13, 10, 0, 0, 0, cstZone)
	if !fireLate.Equal(wantNext) {
		t.Fatalf("got %s want %s", fireLate, wantNext)
	}
}

func TestCheckinAllBothRealmsAndQuotaSync(t *testing.T) {
	var claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case upstream.PathCampaigns:
			if r.Header.Get("Authorization") == "Bearer expired-token" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"code":"TOKEN_EXPIRE"}`)
				return
			}
			_, _ = io.WriteString(w, `{"showCampaign":true,"claimable":true,"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`)
		case "/sash/api/v1/me/campaigns/daily/claim":
			atomic.AddInt32(&claimCalls, 1)
			_, _ = io.WriteString(w, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)
		case upstream.PathProEligibility:
			_, _ = io.WriteString(w, `{"eligible":false}`)
		case upstream.PathDeviceRefresh:
			_, _ = io.WriteString(w, `{"token":"dt-refreshed","refresh_token":"drt-refreshed","expires_in":2592000000}`)
		case upstream.DefaultCreditsPath:
			expSoon := time.Now().Add(48 * time.Hour).Unix()
			_, _ = io.WriteString(w, `{"userQuota":{"remaining":1200,"used":100,"total":1300},"addonQuota":{"remaining":300,"used":0,"total":300,"expiresAt":`+jsonInt(expSoon)+`}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	defer p.Close()

	cnAcct := &auth.Auth{UID: "cn1", Nickname: "CNUser", AccessToken: "expired-token", RefreshToken: "drt-cn", Domain: "qoder.com.cn", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	cnAcct.SetRealm("cn")
	glAcct := &auth.Auth{UID: "gl1", Nickname: "GLUser", AccessToken: "dt-gl", RefreshToken: "drt-gl", Domain: "qoder.com", ExpiresAt: time.Now().Add(48 * time.Hour).Unix()}
	glAcct.SetRealm("global")
	p.Add(cnAcct)
	p.Add(glAcct)

	up := upstream.New()
	up.HTTP = srv.Client()
	up.OpenAPICN = srv.URL
	up.OpenAPIGlobal = srv.URL

	s, err := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{10},
		KeepaliveHours:     []int{4},
		ProBenefitHours:    []int{11},
		ExpiringSoonWindow: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	outcomes, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll failed: %v", err)
	}
	if len(outcomes) != 2 || atomic.LoadInt32(&claimCalls) != 2 {
		t.Fatalf("expected 2 outcomes and 2 claims across CN+Global, got outcomes=%+v claims=%d", outcomes, claimCalls)
	}
	for _, oc := range outcomes {
		if oc.Status != CheckinOK || oc.Credits == nil || *oc.Credits != 1500 {
			t.Fatalf("unexpected outcome: %+v", oc)
		}
	}
	for _, st := range p.List() {
		if st.Credits != 1500 || st.CreditsExpiring != 300 {
			t.Fatalf("pool credits/expiring not updated for %s: %+v", st.UID, st)
		}
	}

	// Second concurrent busy check
	if s.checkinMu.TryLock() {
		defer s.checkinMu.Unlock()
		if _, err := s.CheckinAll(); !errors.Is(err, ErrBusy) {
			t.Fatalf("expected ErrBusy, got %v", err)
		}
	}
}

func TestKeepaliveConsecutiveSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"code":"TOKEN_EXPIRE"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	defer p.Close()

	a := &auth.Auth{UID: "dead1", AccessToken: "dt-1", RefreshToken: "drt-1", Domain: "qoder.com", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	a.SetRealm("global")
	p.Add(a)

	up := upstream.New()
	up.HTTP = srv.Client()
	up.OpenAPIGlobal = srv.URL

	s, _ := New(Config{Pool: p, Upstream: up})
	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		s.RunKeepaliveNow()
	}
	if !p.List()[0].Disabled {
		t.Fatal("expected account disabled after consecutive session_dead keepalives")
	}
}

func jsonInt(v int64) string {
	return strconv.FormatInt(v, 10)
}
