// Package cost is the ember's guard on Tesla's bill. Tesla charges
// developer accounts for streamed signals (150,000 per $1) and for
// billed API calls (data requests at $0.002), and credits $10 a month.
// Going over the billing limit makes Tesla delete the streaming config
// for good — so the ember projects the month, warns at 80% and 100% of
// the credit, and offers "pause streaming".
package cost

import (
	"fmt"
	"math"
	"time"
)

// Tesla's published rates.
const (
	SignalsPerDollar = 150000.0
	DataCallUSD      = 0.002
	CreditUSD        = 10.0
)

// Levels are the warning thresholds, in percent of the credit.
var Levels = []int{80, 100}

// Month is what was used in one calendar month (UTC — Tesla's days and
// the ember's usage keys are UTC).
type Month struct {
	Month       string `json:"month"` // YYYY-MM
	Signals     int64  `json:"signals"`
	BilledCalls int64  `json:"billed_calls"`
	FreeCalls   int64  `json:"free_calls"`
}

// Projection is the month so far and where it is heading.
type Projection struct {
	Month
	CostSoFar    float64 `json:"cost_so_far_usd"`
	Projected    float64 `json:"projected_usd"`
	Credit       float64 `json:"credit_usd"`
	PctSoFar     float64 `json:"pct_so_far"`
	PctProjected float64 `json:"pct_projected"`
	ElapsedDays  float64 `json:"elapsed_days"`
	DaysInMonth  int     `json:"days_in_month"`
	// Reached are the warning levels this month has reached.
	Reached []int `json:"reached"`
}

// MonthKey is the YYYY-MM of t in UTC.
func MonthKey(t time.Time) string { return t.UTC().Format("2006-01") }

// DaysSoFar lists the UTC days of t's month up to and including t.
func DaysSoFar(t time.Time) []string {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	var out []string
	for d := start; !d.After(t); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

// Project turns a month's usage into cost so far and a straight-line
// projection to the month's end.
func Project(m Month, now time.Time) Projection {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	days := int(end.Sub(start).Hours() / 24)
	elapsed := now.Sub(start).Hours() / 24
	p := Projection{Month: m, Credit: CreditUSD, DaysInMonth: days, ElapsedDays: elapsed}
	p.CostSoFar = float64(m.Signals)/SignalsPerDollar + float64(m.BilledCalls)*DataCallUSD
	// never extrapolate from less than a day: the first hours of a month
	// would swing the projection wildly
	frac := math.Max(elapsed, 1) / float64(days)
	p.Projected = p.CostSoFar / math.Min(frac, 1)
	p.PctSoFar = 100 * p.CostSoFar / CreditUSD
	p.PctProjected = 100 * p.Projected / CreditUSD
	for _, lvl := range Levels {
		// a level is reached when the month has actually spent it, or —
		// with two days of history to stand on — is projected to
		if p.PctSoFar >= float64(lvl) || (elapsed >= 2 && p.PctProjected >= float64(lvl)) {
			p.Reached = append(p.Reached, lvl)
		}
	}
	return p
}

// DedupeKey is the notification key: one per month per level.
func DedupeKey(month string, level int) string {
	return fmt.Sprintf("htesla-cost:%s:%d", month, level)
}

// Notice is the bell text for a level. It names no dollar amount other
// than the projection itself.
func Notice(p Projection, level int) (title, body string) {
	if level >= 100 {
		title = "Tesla streaming is on track to use all of this month's credit"
	} else {
		title = fmt.Sprintf("Tesla streaming is at %d%% of this month's credit", level)
	}
	body = fmt.Sprintf("At this pace your car's data stream is projected at $%.2f this month. "+
		"Going past Tesla's billing limit makes Tesla remove the streaming setup for good — "+
		"open the Tesla page to pause streaming or keep an eye on it.", p.Projected)
	return title, body
}
