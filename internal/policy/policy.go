// Package policy decides whether to act on a failed probe. It is a pure
// function of (state, config, now) so every safety rule is unit-testable
// without a cluster or a clock.
package policy

import "time"

type Action string

const (
	None     Action = "None"
	Restart  Action = "Restart"
	Failover Action = "Failover"
)

// Reason explains a decision. It goes into events and the Degraded condition.
type Reason string

const (
	ReasonHealthy           Reason = "Healthy"
	ReasonBelowThreshold    Reason = "BelowThreshold"
	ReasonCoolingDown       Reason = "CoolingDown"
	ReasonRestartBudgetOK   Reason = "RestartBudgetAvailable"
	ReasonBudgetExhausted   Reason = "RestartBudgetExhausted"
	ReasonAlreadyFailedOver Reason = "AlreadyFailedOver"
)

type Config struct {
	FailureThreshold   int
	Cooldown           time.Duration
	MaxRestarts        int
	Window             time.Duration
	FailoverConfigured bool
}

// State is what the controller persists in status between probes.
type State struct {
	ConsecutiveFailures int         // including the probe just taken
	LastRemediation     *time.Time  // nil if never
	RecentRestarts      []time.Time // may contain entries older than Window
	FailedOver          bool
}

type Decision struct {
	Action Action
	Reason Reason
	// RecentRestarts pruned to the window (and extended if Action==Restart).
	// The caller writes it back to status.
	RecentRestarts []time.Time
}

// Decide applies the rules in order:
//  1. healthy, or fewer than FailureThreshold failures in a row -> nothing (flap filter)
//  2. already failed over -> nothing; only a human fails back
//  3. inside the cooldown after the last action -> nothing (let the rollout finish)
//  4. restart budget left in the sliding window -> Restart
//  5. budget spent -> Failover if configured, else nothing (report and stop)
func Decide(healthy bool, s State, c Config, now time.Time) Decision {
	recent := prune(s.RecentRestarts, now, c.Window)
	d := Decision{Action: None, RecentRestarts: recent}

	switch {
	case healthy:
		d.Reason = ReasonHealthy
	case s.ConsecutiveFailures < c.FailureThreshold:
		d.Reason = ReasonBelowThreshold
	case s.FailedOver:
		d.Reason = ReasonAlreadyFailedOver
	case s.LastRemediation != nil && now.Sub(*s.LastRemediation) < c.Cooldown:
		d.Reason = ReasonCoolingDown
	case len(recent) < c.MaxRestarts:
		d.Action, d.Reason = Restart, ReasonRestartBudgetOK
		d.RecentRestarts = append(recent, now)
	case c.FailoverConfigured:
		d.Action, d.Reason = Failover, ReasonBudgetExhausted
	default:
		d.Reason = ReasonBudgetExhausted
	}
	return d
}

func prune(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := make([]time.Time, 0, len(ts)+1)
	for _, t := range ts {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}
