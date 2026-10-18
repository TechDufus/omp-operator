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

package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec, tz string) *Schedule {
	t.Helper()
	s, err := Parse(spec, tz)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", spec, tz, err)
	}
	return s
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, spec, tz string
	}{
		{"every descriptor", "@every 1m", ""},
		{"daily descriptor", "@daily", ""},
		{"CRON_TZ prefix", "CRON_TZ=UTC 0 * * * *", ""},
		{"TZ prefix", "TZ=UTC 0 * * * *", ""},
		{"empty", "  ", ""},
		{"four fields", "* * * *", ""},
		{"six fields", "0 * * * * *", ""},
		{"out of range", "61 * * * *", ""},
		{"bad zone", "0 * * * *", "Mars/Olympus"},
		{"Local zone", "0 * * * *", "Local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.spec, tc.tz); err == nil {
				t.Fatalf("Parse(%q, %q) = nil error, want error", tc.spec, tc.tz)
			}
		})
	}
}

func TestParseDefaultsToUTC(t *testing.T) {
	s := mustParse(t, "0 12 * * *", "")
	if s.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", s.Location())
	}
	got := s.Next(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestNextChicagoAcrossDST(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := mustParse(t, "30 7 * * *", "America/Chicago")
	// US DST began 2026-03-08 (clocks 02:00 -> 03:00): 07:30 is CST (UTC-6)
	// before and CDT (UTC-5) after, so the UTC instant moves by an hour.
	tests := []struct {
		after time.Time
		want  time.Time
	}{
		{time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 7, 13, 30, 0, 0, time.UTC)},
		{time.Date(2026, 3, 7, 13, 30, 0, 0, time.UTC), time.Date(2026, 3, 8, 12, 30, 0, 0, time.UTC)},
		{time.Date(2026, 3, 8, 12, 30, 0, 0, time.UTC), time.Date(2026, 3, 9, 12, 30, 0, 0, time.UTC)},
		// DST ends 2026-11-01 (clocks 02:00 -> 01:00): 07:30 CDT on 10-31, CST after.
		{time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)},
		{time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 13, 30, 0, 0, time.UTC)},
		{time.Date(2026, 11, 1, 13, 30, 0, 0, time.UTC), time.Date(2026, 11, 2, 13, 30, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		got := s.Next(tc.after)
		if !got.Equal(tc.want) {
			t.Errorf("Next(%v) = %v, want %v", tc.after, got, tc.want)
		}
		if got.Location().String() != chicago.String() {
			t.Errorf("Next(%v) location = %v, want America/Chicago", tc.after, got.Location())
		}
		if h, m, _ := got.Clock(); h != 7 || m != 30 {
			t.Errorf("Next(%v) local clock = %02d:%02d, want 07:30", tc.after, h, m)
		}
	}
}

func TestDue(t *testing.T) {
	s := mustParse(t, "*/10 * * * *", "UTC")
	at := func(h, m, sec int) time.Time { return time.Date(2026, 6, 1, h, m, sec, 0, time.UTC) }
	tests := []struct {
		name      string
		last, now time.Time
		want      time.Time
	}{
		{"nothing due", at(10, 0, 0), at(10, 9, 59), time.Time{}},
		{"exactly on slot", at(10, 0, 0), at(10, 10, 0), at(10, 10, 0)},
		{"one missed", at(10, 0, 0), at(10, 12, 0), at(10, 10, 0)},
		{"missed slots collapse to latest", at(10, 0, 0), at(11, 37, 0), at(11, 30, 0)},
		{"last mid-slot", at(10, 5, 30), at(10, 10, 1), at(10, 10, 0)},
		{"already fired latest", at(10, 10, 0), at(10, 19, 59), time.Time{}},
		{"zero last has no baseline", time.Time{}, at(10, 10, 0), time.Time{}},
		{"now before last", at(10, 30, 0), at(10, 0, 0), time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Due(tc.last, tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("Due(%v, %v) = %v, want %v", tc.last, tc.now, got, tc.want)
			}
		})
	}
}

func TestDueAcrossDSTInZone(t *testing.T) {
	if _, err := time.LoadLocation("America/Chicago"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := mustParse(t, "30 7 * * *", "America/Chicago")
	last := time.Date(2026, 3, 7, 13, 30, 0, 0, time.UTC)  // 07:30 CST
	now := time.Date(2026, 3, 10, 12, 31, 0, 0, time.UTC)  // after 07:30 CDT on the 10th
	want := time.Date(2026, 3, 10, 12, 30, 0, 0, time.UTC) // 07:30 CDT
	if got := s.Due(last, now); !got.Equal(want) {
		t.Fatalf("Due = %v, want %v", got, want)
	}
}
