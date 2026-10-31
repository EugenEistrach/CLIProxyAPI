package auth

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var resetAwareNow = time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)

func resetAwareAuth(id string, weekly float64, weeklyResetIn time.Duration, fiveHour float64) *Auth {
	return &Auth{
		ID:    id,
		Label: id,
		Quota: QuotaState{ObservedAt: resetAwareNow.Add(-time.Minute), Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weekly, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAwareNow.Add(weeklyResetIn).Unix(), 10),
			"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(fiveHour, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(resetAwareNow.Add(2*time.Hour).Unix(), 10),
		}},
	}
}

func resetAwarePick(t *testing.T, auths ...*Auth) string {
	t.Helper()
	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }}
	got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func TestResetAwareSelector_UsesTheSubscriptionThatResetsFirstUntilItIsFull(t *testing.T) {
	apple := resetAwareAuth("apple", 0.42, 43*time.Hour, 0.1)
	freshWeek := resetAwareAuth("fresh-week", 0.02, 165*time.Hour, 0.15)
	usedUp := resetAwareAuth("used-up", 1.0, 14*time.Hour, 0)

	for range 50 {
		if got := resetAwarePick(t, freshWeek, usedUp, apple); got != "apple" {
			t.Fatalf("picked %q, want apple: its allowance expires first and a fresh week keeps", got)
		}
	}
}

func TestResetAwareSelector_KeepsUsingASubscriptionUntilItsWeekIsPracticallyUsedUp(t *testing.T) {
	almostDone := resetAwareAuth("almost-done", 0.98, 3*time.Hour, 0.2)
	later := resetAwareAuth("later", 0.1, 100*time.Hour, 0)

	if got := resetAwarePick(t, later, almostDone); got != "almost-done" {
		t.Fatalf("picked %q, want almost-done: its last 2%% expire in 3 hours", got)
	}

	full := resetAwareAuth("full", 0.99, 3*time.Hour, 0.2)
	if got := resetAwarePick(t, later, full); got != "later" {
		t.Fatalf("picked %q, want later once the soonest week is used up", got)
	}
}

func TestResetAwareSelector_ANearlyFullFiveHourWindowHandsOverToTheNextReset(t *testing.T) {
	soonest := resetAwareAuth("soonest", 0.5, 20*time.Hour, 0.92)
	next := resetAwareAuth("next", 0.3, 60*time.Hour, 0.1)
	last := resetAwareAuth("last", 0.0, 160*time.Hour, 0)

	if got := resetAwarePick(t, last, soonest, next); got != "next" {
		t.Fatalf("picked %q, want next while soonest's five-hour window is nearly full", got)
	}
}

func TestResetAwareSelector_AnUnobservedCredentialWaitsBehindSoonerResets(t *testing.T) {
	observed := resetAwareAuth("observed", 0.6, 30*time.Hour, 0.1)
	unobserved := &Auth{ID: "unobserved", Label: "unobserved"}

	if got := resetAwarePick(t, unobserved, observed); got != "observed" {
		t.Fatalf("picked %q, want observed", got)
	}

	busy := resetAwareAuth("busy", 0.6, 30*time.Hour, 0.95)
	if got := resetAwarePick(t, unobserved, busy); got != "unobserved" {
		t.Fatalf("picked %q, want unobserved once the observed one is at its five-hour limit", got)
	}
}

func TestResetAwareSelector_AWindowPastItsResetCountsAsAFreshWeek(t *testing.T) {
	justReset := resetAwareAuth("just-reset", 1.0, -time.Hour, 0)
	midWeek := resetAwareAuth("mid-week", 0.4, 50*time.Hour, 0.1)

	if got := resetAwarePick(t, justReset, midWeek); got != "mid-week" {
		t.Fatalf("picked %q, want mid-week: just-reset now has a full week ahead", got)
	}
}

func TestResetAwareSelector_EveryCredentialNearALimitFallsBackToTheRoomiestWeek(t *testing.T) {
	a := resetAwareAuth("a", 0.995, 10*time.Hour, 0)
	b := resetAwareAuth("b", 0.4, 90*time.Hour, 0.95)

	if got := resetAwarePick(t, a, b); got != "b" {
		t.Fatalf("picked %q, want b, the most weekly allowance left", got)
	}
}

func TestResetAwareSelector_RespectsPriorityTiers(t *testing.T) {
	low := resetAwareAuth("low", 0.5, 10*time.Hour, 0)
	low.Attributes = map[string]string{"priority": "0"}
	high := resetAwareAuth("high", 0.1, 160*time.Hour, 0)
	high.Attributes = map[string]string{"priority": "5"}

	if got := resetAwarePick(t, low, high); got != "high" {
		t.Fatalf("picked %q, want the highest priority tier", got)
	}
}

func TestResetAwareSelector_RecordsEveryCandidateAndWhy(t *testing.T) {
	apple := resetAwareAuth("apple", 0.42, 43*time.Hour, 0.1)
	freshWeek := resetAwareAuth("fresh-week", 0.02, 165*time.Hour, 0.15)
	usedUp := resetAwareAuth("used-up", 1.0, 14*time.Hour, 0)

	resetAwarePick(t, usedUp, freshWeek, apple)

	decision := RecentResetAwareDecisions()[0]
	if decision.ChosenID != "apple" || decision.Rule != "earliest weekly reset with room" {
		t.Fatalf("decision = %+v", decision)
	}
	var order, notes []string
	for _, c := range decision.Candidates {
		order = append(order, c.AuthID)
		notes = append(notes, c.Note)
	}
	if strings.Join(order, ",") != "apple,fresh-week,used-up" {
		t.Fatalf("order = %v, want eligible by earliest reset, then the ineligible", order)
	}
	if notes[0] != "chosen" || !strings.HasPrefix(notes[1], "waits: resets later than apple") || !strings.HasPrefix(notes[2], "weekly allowance used up") {
		t.Fatalf("notes = %q", notes)
	}

	line := resetAwareDecisionLine(decision, resetAwareNow)
	for _, want := range []string{"reset-aware pick: apple (earliest weekly reset with room)", "apple 7d=42%", "5h=10%", "used-up 7d=100%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %q", line, want)
		}
	}
}
