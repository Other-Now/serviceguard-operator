package policy

import (
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func at(sec int) time.Time       { return t0.Add(time.Duration(sec) * time.Second) }
func ptr(t time.Time) *time.Time { return &t }

var base = Config{FailureThreshold: 3, Cooldown: 60 * time.Second, MaxRestarts: 2, Window: 600 * time.Second}

func TestDecideTable(t *testing.T) {
	withFailover := base
	withFailover.FailoverConfigured = true

	cases := []struct {
		name        string
		healthy     bool
		s           State
		c           Config
		now         time.Time
		wantAction  Action
		wantReason  Reason
		wantRecents int
	}{
		{"healthy does nothing", true, State{ConsecutiveFailures: 0}, base, at(0), None, ReasonHealthy, 0},
		{"one failure is noise", false, State{ConsecutiveFailures: 1}, base, at(0), None, ReasonBelowThreshold, 0},
		{"threshold-1 still waits", false, State{ConsecutiveFailures: 2}, base, at(0), None, ReasonBelowThreshold, 0},
		{"threshold reached restarts", false, State{ConsecutiveFailures: 3}, base, at(0), Restart, ReasonRestartBudgetOK, 1},
		{"cooldown blocks", false, State{ConsecutiveFailures: 5, LastRemediation: ptr(at(0)), RecentRestarts: []time.Time{at(0)}}, base, at(59), None, ReasonCoolingDown, 1},
		{"cooldown boundary allows", false, State{ConsecutiveFailures: 5, LastRemediation: ptr(at(0)), RecentRestarts: []time.Time{at(0)}}, base, at(60), Restart, ReasonRestartBudgetOK, 2},
		{"budget spent, no failover: stop", false, State{ConsecutiveFailures: 9, LastRemediation: ptr(at(100)), RecentRestarts: []time.Time{at(0), at(100)}}, base, at(200), None, ReasonBudgetExhausted, 2},
		{"budget spent, failover configured", false, State{ConsecutiveFailures: 9, LastRemediation: ptr(at(100)), RecentRestarts: []time.Time{at(0), at(100)}}, withFailover, at(200), Failover, ReasonBudgetExhausted, 2},
		{"old restarts age out of window", false, State{ConsecutiveFailures: 3, LastRemediation: ptr(at(100)), RecentRestarts: []time.Time{at(0), at(100)}}, base, at(650), Restart, ReasonRestartBudgetOK, 2},
		{"failed over is terminal", false, State{ConsecutiveFailures: 50, FailedOver: true}, withFailover, at(0), None, ReasonAlreadyFailedOver, 0},
		{"zero budget goes straight to failover", false, State{ConsecutiveFailures: 3}, Config{FailureThreshold: 3, Window: time.Minute, FailoverConfigured: true}, at(0), Failover, ReasonBudgetExhausted, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.healthy, tc.s, tc.c, tc.now)
			if d.Action != tc.wantAction || d.Reason != tc.wantReason || len(d.RecentRestarts) != tc.wantRecents {
				t.Fatalf("got %s/%s/%d recents, want %s/%s/%d", d.Action, d.Reason, len(d.RecentRestarts), tc.wantAction, tc.wantReason, tc.wantRecents)
			}
		})
	}
}

// sim drives Decide the way the controller does: one probe per interval,
// failures counted, counter reset after any action.
type sim struct {
	c       Config
	s       State
	actions map[Action]int
}

func (m *sim) step(healthy bool, now time.Time) Action {
	if healthy {
		m.s.ConsecutiveFailures = 0
	} else {
		m.s.ConsecutiveFailures++
	}
	d := Decide(healthy, m.s, m.c, now)
	m.s.RecentRestarts = d.RecentRestarts
	if d.Action != None {
		m.actions[d.Action]++
		m.s.LastRemediation = ptr(now)
		m.s.ConsecutiveFailures = 0
		if d.Action == Failover {
			m.s.FailedOver = true
		}
	}
	return d.Action
}

// A flapping endpoint (up, down, up, down...) must never trigger an action.
func TestFlappingNeverRemediates(t *testing.T) {
	for _, pattern := range [][]bool{
		{true, false},              // 50% alternating
		{false, false, true},       // 2 down, 1 up: always one short of threshold 3
		{true, true, false, false}, // bursts of 2
	} {
		m := &sim{c: base, actions: map[Action]int{}}
		for i := 0; i < 3600/10; i++ { // one hour, 10 s interval
			m.step(pattern[i%len(pattern)], at(i*10))
		}
		if len(m.actions) != 0 {
			t.Fatalf("pattern %v: expected no actions, got %v", pattern, m.actions)
		}
	}
}

// A hard-down endpoint for an hour: restarts are bounded by the budget and
// the rate never exceeds MaxRestarts per Window; then exactly one failover.
func TestHardDownIsBounded(t *testing.T) {
	c := base
	c.FailoverConfigured = true
	m := &sim{c: c, actions: map[Action]int{}}
	var restarts []time.Time
	for i := 0; i < 3600/10; i++ {
		now := at(i * 10)
		if m.step(false, now) == Restart {
			restarts = append(restarts, now)
		}
	}
	if m.actions[Restart] != c.MaxRestarts || m.actions[Failover] != 1 {
		t.Fatalf("want %d restarts then 1 failover, got %v", c.MaxRestarts, m.actions)
	}
	for i := 1; i < len(restarts); i++ {
		if gap := restarts[i].Sub(restarts[i-1]); gap < c.Cooldown {
			t.Fatalf("restarts %v apart, cooldown is %v", gap, c.Cooldown)
		}
	}
}

// Without failover, a hard-down endpoint for 3 hours restarts at most
// MaxRestarts per sliding window, never more.
func TestHardDownNoFailoverRespectsWindow(t *testing.T) {
	m := &sim{c: base, actions: map[Action]int{}}
	var restarts []time.Time
	for i := 0; i < 3*3600/10; i++ {
		now := at(i * 10)
		if m.step(false, now) == Restart {
			restarts = append(restarts, now)
		}
	}
	for i := range restarts {
		n := 0
		for _, r := range restarts {
			if !r.Before(restarts[i]) && r.Sub(restarts[i]) < base.Window {
				n++
			}
		}
		if n > base.MaxRestarts {
			t.Fatalf("%d restarts inside one %v window starting %v", n, base.Window, restarts[i])
		}
	}
	// 3h / 10min window * 2 per window = about 36 at most; well under the 1080 probes.
	if len(restarts) == 0 || len(restarts) > 3*3600/600*base.MaxRestarts {
		t.Fatalf("unexpected restart count %d", len(restarts))
	}
}
