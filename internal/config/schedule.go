// Package config stores shared configuration sections and defaults across commands.
package config

import "fmt"

// Schedule configures periodic CST (UTC+8) tasks: daily campaigns check-in,
// proactive token keepalive refresh, and credit/pro-benefit refresh.
type Schedule struct {
	CheckinHours     []int `json:"checkin_hours"`      // default [10, 22] (Qoder 10:00 CST daily window)
	KeepaliveHours   []int `json:"keepalive_hours"`    // default [4, 16]
	ProBenefitHours  []int `json:"pro_benefit_hours"`  // default [11]
	CheckinEnabled   bool  `json:"checkin_enabled"`    // default true
	KeepaliveEnabled bool  `json:"keepalive_enabled"`  // default true
	ProBenefitEnabled bool `json:"pro_benefit_enabled"` // default true
}

// DefaultSchedule returns Qoder2Api's default CST schedule configuration.
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:      []int{10, 22},
		KeepaliveHours:    []int{4, 16},
		ProBenefitHours:   []int{11},
		CheckinEnabled:    true,
		KeepaliveEnabled:  true,
		ProBenefitEnabled: true,
	}
}

// Normalize fills empty hour slices with defaults and validates 0-23 ranges.
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{10, 22}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{4, 16}
	}
	if len(s.ProBenefitHours) == 0 {
		s.ProBenefitHours = []int{11}
	}
	return s.validateHours()
}

func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	return checkHourRange("schedule.pro_benefit_hours", "pro_benefit_enabled", s.ProBenefitHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d is not a valid hour (0-23); set schedule.%s=false to disable", field, h, switchKey)
		}
	}
	return nil
}
