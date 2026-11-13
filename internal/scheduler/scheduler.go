// Package scheduler runs periodic CST (UTC+8) tasks for Qoder2Api:
//   1. Daily official campaigns check-in (/sash/api/v1/me/campaigns + quota refresh)
//   2. Proactive token keepalive refresh (drt-/jrt-/pt-)
//   3. Pro upgrade/benefit check & credit breakdown sync
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"qoder2api/internal/logfmt"
	"qoder2api/internal/pool"
	"qoder2api/internal/upstream"
)

const (
	checkinRefreshSkew   = 24 * time.Hour
	wakeupLateThreshold  = 1 * time.Second
)

var (
	cstZone          = time.FixedZone("CST", 8*3600)
	wakeupGraceDelay = 5 * time.Second
	// ErrBusy is returned when a manual or scheduled check-in overlaps with one already running.
	ErrBusy = errors.New("checkin already running")
)

// CheckinStatus enumerates per-account check-in outcomes.
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"
	CheckinAlready CheckinStatus = "already"
	CheckinFail    CheckinStatus = "fail"
	CheckinSkipped CheckinStatus = "skipped"
)

// CheckinOutcome captures the result for one account in a CheckinAll run.
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Credits  *float64      `json:"credits,omitempty"`
}

// Config configures the Scheduler loop and tasks.
type Config struct {
	Pool               *pool.Pool
	Upstream           *upstream.Client
	CheckinHours       []int
	KeepaliveHours     []int
	ProBenefitHours    []int
	CheckinDisabled    bool
	KeepaliveDisabled  bool
	ProBenefitDisabled bool
	ExpiringSoonWindow time.Duration
}

// Scheduler dispatches CST scheduled tasks until context cancellation.
type Scheduler struct {
	cfg       Config
	checkinMu sync.Mutex
}

// New constructs a Scheduler validating hour schedules.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Pool == nil || cfg.Upstream == nil {
		return nil, fmt.Errorf("scheduler requires Pool and Upstream")
	}
	if err := validateHours("checkin_hours", cfg.CheckinHours); err != nil {
		return nil, err
	}
	if err := validateHours("keepalive_hours", cfg.KeepaliveHours); err != nil {
		return nil, err
	}
	if err := validateHours("pro_benefit_hours", cfg.ProBenefitHours); err != nil {
		return nil, err
	}
	if cfg.ExpiringSoonWindow <= 0 {
		cfg.ExpiringSoonWindow = 7 * 24 * time.Hour
	}
	return &Scheduler{cfg: cfg}, nil
}

func validateHours(name string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: hour %d out of 0..23", name, h)
		}
	}
	return nil
}

func nextFire(now time.Time, hours []int) time.Time {
	if len(hours) == 0 {
		return time.Time{}
	}
	nowCST := now.In(cstZone)
	var earliest time.Time
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		cand := time.Date(nowCST.Year(), nowCST.Month(), nowCST.Day(), h, 0, 0, 0, cstZone)
		if !cand.After(nowCST) {
			cand = cand.Add(24 * time.Hour)
		}
		if earliest.IsZero() || cand.Before(earliest) {
			earliest = cand
		}
	}
	if earliest.IsZero() {
		return time.Time{}
	}
	return earliest.In(now.Location())
}

type taskKind int

const (
	taskCheckin taskKind = iota
	taskKeepalive
	taskProBenefit
)

func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskKeepalive:
		return "keepalive"
	case taskProBenefit:
		return "pro_benefit"
	default:
		return fmt.Sprintf("task(%d)", int(k))
	}
}

func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if !s.cfg.ProBenefitDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ProBenefitHours), taskProBenefit})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if time.Since(planned) <= wakeupLateThreshold {
		return ctx.Err() == nil
	}
	log.Printf("[scheduler] wakeup grace %s for late slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// Run blocks executing scheduled CST tasks until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			<-ctx.Done()
			return
		}
		log.Printf("[scheduler] next wake at %s (tasks=%v, wait=%s)",
			next.Format("2006-01-02 15:04:05"), kinds, time.Until(next).Round(time.Second))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if !awaitWakeupGrace(ctx, next) {
				return
			}
			log.Printf("[scheduler] executing tasks: %v", kinds)
			s.runBatch(ctx, kinds)
		}
	}
}

func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskProBenefit:
				s.RunProBenefitNow(ctx)
			}
		}(k)
	}
	wg.Wait()
}

// RunCheckinNow runs scheduled CheckinAll logging busy contention gracefully.
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("[scheduler] scheduled checkin skipped: %v", err)
	}
}

// CheckinAll runs Qoder official campaign check-in and credit quota refresh across CN & Global accounts.
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int

	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || (a.RefreshTokenValue() == "" && a.PersonalTokenValue() == "" && a.AccessTokenValue() == "") {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}

		// Proactively refresh near-expiry or missing access tokens before check-in.
		if a.NeedsRefresh(checkinRefreshSkew) && (a.RefreshTokenValue() != "" || a.PersonalTokenValue() != "") {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("[checkin] %s pre-refresh failed: %v", logfmt.Label(st.UID, st.Nickname), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: [checkin] %s disabled after consecutive session_dead", logfmt.Label(st.UID, st.Nickname))
					}
				}
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				s.cfg.Pool.ClearSessionDead(st.UID)
				a.BackfillRealm()
				_ = a.SaveAtomic()
			}
		}

		// Execute Qoder campaign check-in (retry once after forced token refresh on 401/session_dead).
		checkinErr := s.cfg.Upstream.DailyCheckin(a)
		if checkinErr != nil {
			var ue *upstream.Error
			if errors.As(checkinErr, &ue) && (ue.Status == 401 || ue.Kind == upstream.ErrSessionDead) && (a.RefreshTokenValue() != "" || a.PersonalTokenValue() != "") {
				if rerr := s.cfg.Upstream.RefreshToken(a); rerr == nil {
					a.BackfillRealm()
					_ = a.SaveAtomic()
					checkinErr = s.cfg.Upstream.DailyCheckin(a)
				}
			}
		}

		if checkinErr != nil {
			if upstream.IsAlreadyCheckin(checkinErr) {
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = checkinErr.Error()
				log.Printf("[checkin] %s: %v", logfmt.Label(st.UID, st.Nickname), checkinErr)
			}
		} else {
			oc.Status = CheckinOK
		}

		// Refresh credit balance and 7d expiring slice; reactivate previously exhausted accounts if topped up.
		remain, detail, qerr := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if qerr != nil {
			oc.Detail = joinDetail(oc.Detail, "quota: "+qerr.Error())
			if oc.Status == CheckinOK || oc.Status == CheckinAlready {
				// Check-in succeeded even if quota endpoint varies (e.g. Global probe temporarily failed); preserve status.
				if oc.Status == CheckinOK {
					okN++
				} else {
					alreadyN++
				}
			} else {
				oc.Status = CheckinFail
				failN++
			}
			s.cfg.Pool.SetCheckinResult(st.UID, string(oc.Status), oc.Detail)
			out = append(out, oc)
			continue
		}

		expiring := 0.0
		if detail != nil {
			expiring = detail.Expiring
		}
		remInt := int64(remain)
		expInt := int64(expiring)
		s.cfg.Pool.ReenableIfCredits(st.UID, remInt)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remInt, expInt)
		oc.Credits = &remain

		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		s.cfg.Pool.SetCheckinResult(st.UID, string(oc.Status), oc.Detail)
		out = append(out, oc)
	}

	for _, oc := range out {
		if oc.Status == CheckinSkipped || oc.Status == CheckinFail {
			s.cfg.Pool.SetCheckinResult(oc.UID, string(oc.Status), oc.Detail)
		}
	}
	s.cfg.Pool.SetCheckinRunSummary("auto", len(statuses), okN, alreadyN, failN, skipN)
	log.Printf("[checkin] done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// RunKeepaliveNow proactively refreshes OAuth/PAT credentials across all active accounts.
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || (a.RefreshTokenValue() == "" && a.PersonalTokenValue() == "") {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("[keepalive] %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: [keepalive] %s: consecutive %d session_dead failures — disabled",
						logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID)
		a.BackfillRealm()
		if err := a.SaveAtomic(); err != nil {
			log.Printf("[keepalive] %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunProBenefitNow checks Pro upgrade eligibility and syncs fresh credit breakdowns.
func (s *Scheduler) RunProBenefitNow(ctx context.Context) {
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		claimed, _ := s.cfg.Upstream.CheckAndClaimProUpgrade(ctx, a)
		if claimed {
			log.Printf("[pro-benefit] %s claimed Pro upgrade!", logfmt.Label(st.UID, st.Nickname))
		}
		if remain, detail, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow); err == nil {
			exp := 0.0
			if detail != nil {
				exp = detail.Expiring
			}
			remInt := int64(remain)
			expInt := int64(exp)
			s.cfg.Pool.ReenableIfCredits(st.UID, remInt)
			s.cfg.Pool.SetCreditsDetailed(st.UID, remInt, expInt)
		}
	}
}

func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
