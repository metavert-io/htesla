package cost

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestProjection(t *testing.T) {
	// 10 days into a 31-day month, 300,000 signals = $2.00 so far
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	p := Project(Month{Month: "2026-10", Signals: 300000}, now)
	if math.Abs(p.CostSoFar-2.0) > 1e-9 || p.DaysInMonth != 31 || math.Abs(p.ElapsedDays-10) > 1e-9 {
		t.Fatalf("%+v", p)
	}
	if math.Abs(p.Projected-6.2) > 1e-9 || math.Abs(p.PctProjected-62) > 1e-6 || len(p.Reached) != 0 {
		t.Fatalf("projected %.4f (%.1f%%) reached %v", p.Projected, p.PctProjected, p.Reached)
	}
	// billed data calls count at $0.002
	p2 := Project(Month{Month: "2026-10", Signals: 0, BilledCalls: 500}, now)
	if math.Abs(p2.CostSoFar-1.0) > 1e-9 {
		t.Fatalf("calls %.4f", p2.CostSoFar)
	}
	// 80%: projected $8.06 after 10 days
	p3 := Project(Month{Month: "2026-10", Signals: 390000}, now)
	if len(p3.Reached) != 1 || p3.Reached[0] != 80 {
		t.Fatalf("want 80 reached, got %v (%.1f%%)", p3.Reached, p3.PctProjected)
	}
	// already over the credit
	p4 := Project(Month{Month: "2026-10", Signals: 1600000}, now)
	if len(p4.Reached) != 2 {
		t.Fatalf("want 80 and 100, got %v", p4.Reached)
	}
}

func TestEarlyMonthDoesNotPanic(t *testing.T) {
	// 3 hours into the month: the projection is held to "a day's worth"
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	p := Project(Month{Month: "2026-11", Signals: 30000}, now) // $0.20
	if math.Abs(p.Projected-0.2*30) > 1e-9 {
		t.Fatalf("projected %.2f", p.Projected)
	}
	// a big first day projects past the credit, but with under two days
	// of history only the ACTUAL spend can raise a level
	p2 := Project(Month{Month: "2026-11", Signals: 60000}, now) // $0.40 → $12 projected
	if p2.PctProjected < 100 || len(p2.Reached) != 0 {
		t.Fatalf("%v %.0f%%", p2.Reached, p2.PctProjected)
	}
}

func TestDedupeAndNoticeText(t *testing.T) {
	if DedupeKey("2026-10", 80) != "htesla-cost:2026-10:80" || DedupeKey("2026-10", 100) != "htesla-cost:2026-10:100" {
		t.Fatal("dedupe keys")
	}
	p := Project(Month{Month: "2026-10", Signals: 390000}, time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	title, body := Notice(p, 80)
	amounts := regexp.MustCompile(`\$[0-9.,]+`).FindAllString(title+" "+body, -1)
	if len(amounts) != 1 || amounts[0] != "$8.06" {
		t.Fatalf("the only dollar figure must be the projection: %v\n%s\n%s", amounts, title, body)
	}
	if !strings.Contains(title, "80%") {
		t.Fatal(title)
	}
}

func TestDaysSoFar(t *testing.T) {
	d := DaysSoFar(time.Date(2026, 10, 3, 23, 0, 0, 0, time.UTC))
	if len(d) != 3 || d[0] != "2026-10-01" || d[2] != "2026-10-03" || MonthKey(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) != "2026-10" {
		t.Fatal(d)
	}
}
