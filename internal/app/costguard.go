package app

// Item 14: the cost guard. Tesla bills streamed signals and data calls
// against a $10 monthly credit, and going over the billing limit makes
// Tesla delete the streaming config for good. Every few minutes this
// sums the month's signals (KV usage/<day>) and metered API calls,
// projects the month, publishes it (KV "cost" + status), and rings the
// bell once per month at 80% and 100% of the credit.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/metavert-io/htesla/internal/cost"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/store"
	"github.com/metavert-io/htesla/internal/tesla"
)

const (
	KVCost   = "cost"
	KVAlerts = "cost_alerts"
)

// callCount is one month of metered Fleet API calls (KV api_calls/<month>).
type callCount struct {
	Month  string           `json:"month"`
	Free   int64            `json:"free"`
	Billed int64            `json:"billed"`
	ByPath map[string]int64 `json:"by_endpoint,omitempty"`
	loaded bool
}

func (a *App) costLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	a.costPass(ctx)
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.costPass(ctx)
		}
	}
}

func (a *App) monthCalls(ctx context.Context, month string) *callCount {
	cc := a.calls[month]
	if cc == nil {
		cc = &callCount{Month: month}
		a.calls[month] = cc
	}
	if !cc.loaded {
		var prev callCount
		err := a.H.KVGetJSON(ctx, "api_calls/"+month, &prev)
		if err == nil || errors.Is(err, hearth.ErrNotFound) {
			cc.Free += prev.Free
			cc.Billed += prev.Billed
			if cc.ByPath == nil {
				cc.ByPath = map[string]int64{}
			}
			for k, v := range prev.ByPath {
				cc.ByPath[k] += v
			}
			cc.loaded = true
		}
	}
	return cc
}

// costPass recomputes the month and warns when it should.
func (a *App) costPass(ctx context.Context) {
	now := time.Now()
	month := cost.MonthKey(now)
	m := cost.Month{Month: month}
	today := now.UTC().Format("2006-01-02")
	var todayStored int64
	for _, day := range cost.DaysSoFar(now) {
		var u store.Usage
		if err := a.H.KVGetJSON(ctx, store.UsageKey(day), &u); err == nil {
			m.Signals += u.Signals
			if day == today {
				todayStored = u.Signals
			}
		}
	}
	a.mu.Lock()
	a.todayStored = map[string]int64{today: todayStored}
	a.mu.Unlock()
	// this process's calls since the last pass join the month's tally
	free, billed, by := a.meter.Take()
	cc := a.monthCalls(ctx, month)
	if free+billed > 0 {
		cc.Free += free
		cc.Billed += billed
		if cc.ByPath == nil {
			cc.ByPath = map[string]int64{}
		}
		for k, v := range by {
			cc.ByPath[k] += v
		}
		if cc.loaded {
			if err := a.H.KVPutJSON(ctx, "api_calls/"+month, cc); err != nil {
				a.logf("saving the API call tally: %v", err)
			}
		}
	}
	m.FreeCalls, m.BilledCalls = cc.Free, cc.Billed
	p := cost.Project(m, now)
	if err := a.H.KVPutJSON(ctx, KVCost, p); err != nil {
		a.logf("saving the cost projection: %v", err)
	}
	a.update(func(s *Status) { s.Cost = &p })
	a.costAlerts(ctx, p)
}

func (a *App) costAlerts(ctx context.Context, p cost.Projection) {
	if len(p.Reached) == 0 {
		return
	}
	if !a.alertsOK {
		var prev map[string]string
		err := a.H.KVGetJSON(ctx, KVAlerts, &prev)
		if err != nil && !errors.Is(err, hearth.ErrNotFound) {
			return // can't tell what was already sent: don't risk a repeat
		}
		for k, v := range prev {
			a.alerts[k] = v
		}
		a.alertsOK = true
	}
	for _, lvl := range p.Reached {
		key := cost.DedupeKey(p.Month.Month, lvl)
		if _, sent := a.alerts[key]; sent {
			continue
		}
		title, body := cost.Notice(p, lvl)
		sev := "warn"
		if lvl >= 100 {
			sev = "error"
		}
		if err := a.notifyExact(ctx, key, hearth.Notification{
			Severity: sev, Title: title, Body: body,
			Link: "#/plugin/htesla/index.html", LinkLabel: "Open Tesla",
		}); err != nil {
			a.logf("cost notification: %v", err)
			continue
		}
		a.alerts[key] = now()
		b, _ := json.Marshal(a.alerts)
		if err := a.H.KVPut(ctx, KVAlerts, string(b)); err != nil {
			a.logf("saving cost alerts: %v", err)
		}
	}
}

// pauseStreaming removes the streaming config from every car (the free
// DELETE fleet_telemetry_config); "resume" sends a fresh one.
func (a *App) pauseStreaming(ctx context.Context) {
	a.mu.Lock()
	vins := append([]string(nil), a.st.Streaming.VINs...)
	a.mu.Unlock()
	if len(vins) == 0 {
		a.update(func(s *Status) { s.Streaming.Error = "Nothing is streaming yet." })
		return
	}
	var failed []string
	for _, vin := range vins {
		err := a.withUser(ctx, func(f *tesla.Fleet) error { return f.DeleteTelemetryConfig(ctx, vin) })
		if err != nil {
			failed = append(failed, "…"+last4(vin)+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		a.update(func(s *Status) {
			s.Streaming.Error = "Couldn't pause every car: " + failed[0]
		})
		return
	}
	a.logf("streaming paused (telemetry config removed from %d car(s))", len(vins))
	a.update(func(s *Status) {
		s.Streaming.State = "paused"
		s.Streaming.PausedAt = now()
		s.Streaming.Error = ""
		s.Streaming.PerVIN = map[string]*VINStream{}
	})
}
