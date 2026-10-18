/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package schedule parses 5-field cron expressions and computes fire slots in
// a time zone. It is pure: no clocks, no Kubernetes.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Schedule is a parsed cron expression bound to a time zone.
type Schedule struct {
	sched cron.Schedule
	loc   *time.Location
}

// Parse parses a standard 5-field cron expression (minute hour dom month dow)
// evaluated in the IANA zone tz (empty means UTC). Descriptors such as @every
// or @daily and TZ=/CRON_TZ= prefixes are rejected.
func Parse(spec, tz string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("cron expression is empty")
	}
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return nil, errors.New("TZ= and CRON_TZ= prefixes are not allowed; use timeZone")
	}
	if strings.HasPrefix(spec, "@") {
		return nil, errors.New("descriptors such as @every and @daily are not allowed; use 5 cron fields")
	}
	if n := len(strings.Fields(spec)); n != 5 {
		return nil, fmt.Errorf("cron expression must have exactly 5 fields, got %d", n)
	}
	if tz == "" {
		tz = "UTC"
	}
	if tz == "Local" {
		return nil, errors.New(`time zone "Local" is not allowed; use an IANA name`)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid time zone %q: %w", tz, err)
	}
	sched, err := parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	return &Schedule{sched: sched, loc: loc}, nil
}

// Location returns the schedule's time zone.
func (s *Schedule) Location() *time.Location { return s.loc }

// Next returns the first slot strictly after the given instant, in the
// schedule's location, or the zero time when none exists within the cron
// library's search horizon.
func (s *Schedule) Next(after time.Time) time.Time {
	return s.sched.Next(after.In(s.loc))
}

// Due returns the latest slot t with lastFire < t <= now, in the schedule's
// location. Missed slots collapse into that single latest one. It returns the
// zero time when nothing is due, and also when lastFire is zero: callers
// supply a baseline such as the trigger's creation time so a new trigger does
// not fire for slots that predate it.
func (s *Schedule) Due(lastFire, now time.Time) time.Time {
	if lastFire.IsZero() {
		return time.Time{}
	}
	var due time.Time
	for t := s.Next(lastFire); !t.IsZero() && !t.After(now); t = s.Next(t) {
		due = t
	}
	return due
}
