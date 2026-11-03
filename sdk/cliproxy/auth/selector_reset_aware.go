package auth

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ResetAwareSelector keeps every subscription on pace with its own week. A subscription's pace
// is the share of its weekly window that has passed; a new conversation goes to the subscription
// furthest behind its pace (pace minus weekly usage). The weight therefore moves to a
// subscription as its reset approaches with allowance left, a freshly reset one waits, and none
// is drained ahead of the others. When demand is below what the subscriptions can serve, the
// allowance left unused is the same whichever one serves a request; spreading it by pace keeps
// headroom on every subscription instead of emptying one.
//
// A reserve keeps a share of one subscription's week for use outside this proxy (the Claude app
// on the same account): this proxy stops using it at 1 - share until ReleaseBefore its reset,
// when the reserve is released so it isn't lost at the reset. A subscription at its limit (the
// weekly ceiling, its reserve, or a nearly full five-hour window) also loses its running
// conversations: SessionAffinitySelector drops it from the candidates (HoldsBack).
//
// The usage comes from the Anthropic unified rate-limit signals the host records per credential
// (response headers and the usage poller). A window whose reset has passed counts as fresh. A
// credential never observed counts as resetting a full week from now (pace 0). Every pick is
// recorded with each candidate's numbers and the reason it was or wasn't chosen
// (RecentResetAwareDecisions, and the log).
type ResetAwareSelector struct {
	now     func() time.Time
	Reserve ResetAwareReserve
}

// ResetAwareReserve keeps Share of Account's weekly allowance away from this proxy until
// ReleaseBefore its weekly reset. Account matches a credential's label (email) or ID.
type ResetAwareReserve struct {
	Account       string
	Share         float64
	ReleaseBefore time.Duration
}

const (
	resetAwareSignalPrefix    = "Anthropic-Ratelimit-Unified-"
	resetAwareFiveHourCeiling = 0.9
	resetAwareWeeklyCeiling   = 0.99
	resetAwareFiveHourWindow  = 5 * time.Hour
	resetAwareWeeklyWindow    = 7 * 24 * time.Hour
	resetAwareDecisionsKept   = 200
)

type resetAwareWindow struct {
	utilization float64
	resetsAt    time.Time
	observed    bool
}

// observedResetAwareWindow reads one unified window ("5h" or "7d") from a credential's signals.
// A window whose reset has passed counts as fresh, since its usage no longer applies; a window
// without signals counts as unused and resetting one window length from now.
func observedResetAwareWindow(auth *Auth, window string, length time.Duration, now time.Time) resetAwareWindow {
	unobserved := resetAwareWindow{utilization: 0, resetsAt: now.Add(length)}
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return unobserved
	}
	utilization, errUtilization := strconv.ParseFloat(strings.TrimSpace(auth.Quota.Signals[resetAwareSignalPrefix+window+"-Utilization"]), 64)
	resetSeconds, errReset := strconv.ParseInt(strings.TrimSpace(auth.Quota.Signals[resetAwareSignalPrefix+window+"-Reset"]), 10, 64)
	if errUtilization != nil || errReset != nil || utilization < 0 || resetSeconds <= 0 {
		return unobserved
	}
	resetsAt := time.Unix(resetSeconds, 0)
	if !resetsAt.After(now) {
		return resetAwareWindow{utilization: 0, resetsAt: now.Add(length), observed: true}
	}
	return resetAwareWindow{utilization: utilization, resetsAt: resetsAt, observed: true}
}

// ResetAwareCandidate is one credential as the selector saw it during a pick.
type ResetAwareCandidate struct {
	AuthID            string    `json:"auth_id"`
	Label             string    `json:"label"`
	WeeklyUtilization float64   `json:"weekly_utilization"`
	WeeklyResetsAt    time.Time `json:"weekly_resets_at"`
	WeeklyObserved    bool      `json:"weekly_observed"`
	WeeklyPace        float64   `json:"weekly_pace"`
	WeeklyCap         float64   `json:"weekly_cap"`
	BehindPace        float64   `json:"behind_pace"`
	Reserved          bool      `json:"reserved"`
	FiveHourUsage     float64   `json:"five_hour_utilization"`
	FiveHourObserved  bool      `json:"five_hour_observed"`
	SignalsObservedAt time.Time `json:"signals_observed_at"`
	Eligible          bool      `json:"eligible"`
	Note              string    `json:"note"`
}

// ResetAwareDecision records one pick: what was chosen, why, and every candidate in the order considered.
type ResetAwareDecision struct {
	At         time.Time             `json:"at"`
	Provider   string                `json:"provider"`
	Model      string                `json:"model"`
	ChosenID   string                `json:"chosen_auth_id"`
	Rule       string                `json:"rule"`
	Candidates []ResetAwareCandidate `json:"candidates"`
}

var resetAwareDecisions = struct {
	mu    sync.Mutex
	items []ResetAwareDecision
}{}

func recordResetAwareDecision(decision ResetAwareDecision) {
	resetAwareDecisions.mu.Lock()
	defer resetAwareDecisions.mu.Unlock()
	resetAwareDecisions.items = append(resetAwareDecisions.items, decision)
	if overflow := len(resetAwareDecisions.items) - resetAwareDecisionsKept; overflow > 0 {
		resetAwareDecisions.items = append([]ResetAwareDecision(nil), resetAwareDecisions.items[overflow:]...)
	}
}

// RecentResetAwareDecisions returns the most recent picks, newest first.
func RecentResetAwareDecisions() []ResetAwareDecision {
	resetAwareDecisions.mu.Lock()
	defer resetAwareDecisions.mu.Unlock()
	out := make([]ResetAwareDecision, 0, len(resetAwareDecisions.items))
	for i := len(resetAwareDecisions.items) - 1; i >= 0; i-- {
		out = append(out, resetAwareDecisions.items[i])
	}
	return out
}

func resetAwareLabel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if email := strings.TrimSpace(auth.Label); email != "" {
		return email
	}
	return auth.ID
}

func (r ResetAwareReserve) appliesTo(auth *Auth) bool {
	account := strings.TrimSpace(r.Account)
	if account == "" || r.Share <= 0 || auth == nil {
		return false
	}
	return strings.EqualFold(account, strings.TrimSpace(auth.Label)) || account == auth.ID
}

func (s *ResetAwareSelector) candidateOf(auth *Auth, now time.Time) ResetAwareCandidate {
	weekly := observedResetAwareWindow(auth, "7d", resetAwareWeeklyWindow, now)
	fiveHour := observedResetAwareWindow(auth, "5h", resetAwareFiveHourWindow, now)
	untilReset := weekly.resetsAt.Sub(now)
	pace := math.Min(1, math.Max(0, 1-float64(untilReset)/float64(resetAwareWeeklyWindow)))
	weeklyCap := resetAwareWeeklyCeiling
	reserved := false
	if s.Reserve.appliesTo(auth) && untilReset > s.Reserve.ReleaseBefore {
		weeklyCap = math.Min(weeklyCap, 1-s.Reserve.Share)
		reserved = true
	}
	candidate := ResetAwareCandidate{
		AuthID:            auth.ID,
		Label:             resetAwareLabel(auth),
		WeeklyUtilization: weekly.utilization,
		WeeklyResetsAt:    weekly.resetsAt,
		WeeklyObserved:    weekly.observed,
		WeeklyPace:        pace,
		WeeklyCap:         weeklyCap,
		BehindPace:        pace*weeklyCap - weekly.utilization,
		Reserved:          reserved,
		FiveHourUsage:     fiveHour.utilization,
		FiveHourObserved:  fiveHour.observed,
		SignalsObservedAt: auth.Quota.ObservedAt,
		Eligible:          true,
	}
	switch {
	case reserved && weekly.utilization >= weeklyCap:
		candidate.Eligible = false
		candidate.Note = fmt.Sprintf("reserve held (%.0f%% >= %.0f%% until %s before reset)", weekly.utilization*100, weeklyCap*100, s.Reserve.ReleaseBefore)
	case weekly.utilization >= weeklyCap:
		candidate.Eligible = false
		candidate.Note = fmt.Sprintf("weekly allowance used up (%.0f%% >= %.0f%%)", weekly.utilization*100, weeklyCap*100)
	case fiveHour.utilization >= resetAwareFiveHourCeiling:
		candidate.Eligible = false
		candidate.Note = fmt.Sprintf("five-hour window nearly full (%.0f%% >= %.0f%%)", fiveHour.utilization*100, resetAwareFiveHourCeiling*100)
	case !weekly.observed:
		candidate.Note = "no weekly usage observed yet; counts as resetting in 7 days"
	}
	return candidate
}

// HoldsBack reports whether a credential is at one of its limits, so even conversations already
// bound to it move to another credential.
func (s *ResetAwareSelector) HoldsBack(auth *Auth) bool {
	if auth == nil {
		return false
	}
	return !s.candidateOf(auth, s.currentTime()).Eligible
}

// resetAwareOrder sorts candidates into the order they are considered: eligible ones by how far
// they are behind their pace (ties: earliest weekly reset, then ID), then the ineligible ones.
func resetAwareOrder(candidates []ResetAwareCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.BehindPace != b.BehindPace {
			return a.BehindPace > b.BehindPace
		}
		if !a.WeeklyResetsAt.Equal(b.WeeklyResetsAt) {
			return a.WeeklyResetsAt.Before(b.WeeklyResetsAt)
		}
		return a.AuthID < b.AuthID
	})
}

func (s *ResetAwareSelector) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Pick chooses the eligible credential furthest behind its weekly pace. When every credential is
// at a limit, the one with the most room under its cap is used, so requests still go out and the
// host's own cooldowns handle an actual rejection.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.currentTime()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	byID := make(map[string]*Auth, len(available))
	candidates := make([]ResetAwareCandidate, 0, len(available))
	for _, auth := range available {
		byID[auth.ID] = auth
		candidates = append(candidates, s.candidateOf(auth, now))
	}
	resetAwareOrder(candidates)

	chosen := candidates[0]
	rule := "furthest behind weekly pace"
	if !chosen.Eligible {
		rule = "every credential at a limit: most room under its cap"
		for _, candidate := range candidates[1:] {
			if candidate.WeeklyCap-candidate.WeeklyUtilization > chosen.WeeklyCap-chosen.WeeklyUtilization {
				chosen = candidate
			}
		}
	}
	for i := range candidates {
		if candidates[i].AuthID == chosen.AuthID && candidates[i].Note == "" {
			candidates[i].Note = "chosen"
		} else if candidates[i].Eligible && candidates[i].Note == "" {
			candidates[i].Note = fmt.Sprintf("waits: less behind pace than %s", chosen.Label)
		}
	}

	decision := ResetAwareDecision{At: now, Provider: provider, Model: model, ChosenID: chosen.AuthID, Rule: rule, Candidates: candidates}
	recordResetAwareDecision(decision)
	log.Info(resetAwareDecisionLine(decision, now))
	return byID[chosen.AuthID], nil
}

func resetAwareDecisionLine(decision ResetAwareDecision, now time.Time) string {
	var b strings.Builder
	chosenLabel := decision.ChosenID
	for _, candidate := range decision.Candidates {
		if candidate.AuthID == decision.ChosenID {
			chosenLabel = candidate.Label
		}
	}
	fmt.Fprintf(&b, "reset-aware pick: %s (%s) model=%s |", chosenLabel, decision.Rule, decision.Model)
	for _, c := range decision.Candidates {
		weekly := "7d=?"
		if c.WeeklyObserved {
			weekly = fmt.Sprintf("7d=%.0f%%", c.WeeklyUtilization*100)
		}
		fiveHour := "5h=?"
		if c.FiveHourObserved {
			fiveHour = fmt.Sprintf("5h=%.0f%%", c.FiveHourUsage*100)
		}
		age := "never"
		if !c.SignalsObservedAt.IsZero() {
			age = now.Sub(c.SignalsObservedAt).Round(time.Minute).String() + " ago"
		}
		capLabel := fmt.Sprintf("cap %.0f%%", c.WeeklyCap*100)
		if c.Reserved {
			capLabel += " (reserve)"
		}
		fmt.Fprintf(&b, " %s %s pace %.0f%% behind %+.0f %s resets %s (in %s) %s seen %s: %s;", c.Label, weekly,
			c.WeeklyPace*100, c.BehindPace*100, capLabel,
			c.WeeklyResetsAt.Local().Format("Mon 02 15:04"), c.WeeklyResetsAt.Sub(now).Round(time.Minute), fiveHour, age, c.Note)
	}
	return strings.TrimSuffix(b.String(), ";")
}
