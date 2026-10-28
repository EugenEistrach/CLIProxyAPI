package auth

import (
	"context"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var resetAwareNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func resetAwareAuth(id string, weekly float64, weeklyResetIn time.Duration, fiveHour float64) *Auth {
	return &Auth{
		ID: id,
		Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weekly, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAwareNow.Add(weeklyResetIn).Unix(), 10),
			"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(fiveHour, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(resetAwareNow.Add(2*time.Hour).Unix(), 10),
		}},
	}
}

func resetAwareShares(t *testing.T, auths []*Auth, picks int) map[string]float64 {
	t.Helper()
	selector := &ResetAwareSelector{rand: rand.New(rand.NewPCG(1, 2)), now: func() time.Time { return resetAwareNow }}
	counts := make(map[string]int)
	for range picks {
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[got.ID]++
	}
	shares := make(map[string]float64, len(counts))
	for id, count := range counts {
		shares[id] = float64(count) / float64(picks)
	}
	return shares
}

func TestResetAwareSelector_SharesFollowAllowanceLeftPerHourUntilReset(t *testing.T) {
	t.Parallel()

	shares := resetAwareShares(t, []*Auth{
		resetAwareAuth("main", 0.68, 67*time.Hour, 0),
		resetAwareAuth("apple", 0.07, 113*time.Hour, 0),
	}, 20000)

	mainUrgency := 0.32 / 67
	appleUrgency := 0.93 / 113
	wantApple := appleUrgency / (mainUrgency + appleUrgency)
	if math.Abs(shares["apple"]-wantApple) > 0.02 {
		t.Fatalf("apple share = %.3f, want about %.3f", shares["apple"], wantApple)
	}
	if shares["main"] < 0.3 {
		t.Fatalf("main share = %.3f, want it to keep a real share", shares["main"])
	}
}

func TestResetAwareSelector_AllowanceAboutToExpireOutweighsAFreshWeek(t *testing.T) {
	t.Parallel()

	shares := resetAwareShares(t, []*Auth{
		resetAwareAuth("fresh-week", 0, 7*24*time.Hour, 0),
		resetAwareAuth("expires-monday", 0.5, 46*time.Hour, 0),
	}, 20000)

	want := (0.5 / 46) / (0.5/46 + 1.0/168)
	if math.Abs(shares["expires-monday"]-want) > 0.02 {
		t.Fatalf("expires-monday share = %.3f, want about %.3f", shares["expires-monday"], want)
	}
}

func TestResetAwareSelector_CredentialsNearALimitGetNoNewSelections(t *testing.T) {
	t.Parallel()

	shares := resetAwareShares(t, []*Auth{
		resetAwareAuth("weekly-nearly-used", 0.98, 10*time.Hour, 0),
		resetAwareAuth("five-hour-nearly-used", 0.1, 100*time.Hour, 0.92),
		resetAwareAuth("roomy", 0.5, 100*time.Hour, 0.2),
	}, 2000)

	if shares["roomy"] != 1 {
		t.Fatalf("shares = %v, want every selection on roomy", shares)
	}
}

func TestResetAwareSelector_FiveHourUsageEasesLoadOff(t *testing.T) {
	t.Parallel()

	shares := resetAwareShares(t, []*Auth{
		resetAwareAuth("busy", 0.3, 100*time.Hour, 0.8),
		resetAwareAuth("idle", 0.3, 100*time.Hour, 0),
	}, 20000)

	if math.Abs(shares["busy"]-0.2/1.2) > 0.02 {
		t.Fatalf("busy share = %.3f, want about %.3f", shares["busy"], 0.2/1.2)
	}
}

func TestResetAwareSelector_UnobservedCredentialCountsAsHalfUsedWithAFullWeek(t *testing.T) {
	t.Parallel()

	unobserved := &Auth{ID: "unobserved"}
	if got, want := resetAwareWeight(unobserved, resetAwareNow), 0.5/(7*24)*0.5; math.Abs(got-want) > 1e-12 {
		t.Fatalf("resetAwareWeight(unobserved) = %v, want %v", got, want)
	}
	if resetAwareWeight(unobserved, resetAwareNow) <= 0 {
		t.Fatalf("an unobserved credential must still be tried")
	}
}

func TestResetAwareSelector_WindowPastItsResetCountsAsFresh(t *testing.T) {
	t.Parallel()

	reset := resetAwareAuth("reset", 1, -time.Minute, 1)
	reset.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(resetAwareNow.Add(-time.Minute).Unix(), 10)

	if got, want := resetAwareWeight(reset, resetAwareNow), 1.0/(7*24); math.Abs(got-want) > 1e-12 {
		t.Fatalf("resetAwareWeight(after reset) = %v, want %v", got, want)
	}
}

func TestResetAwareSelector_EveryCredentialNearALimitFallsBackToTheRoomiestWeek(t *testing.T) {
	t.Parallel()

	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{
		resetAwareAuth("a", 0.99, 10*time.Hour, 0),
		resetAwareAuth("b", 0.4, 10*time.Hour, 0.95),
		resetAwareAuth("c", 0.98, 10*time.Hour, 0),
	})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "b")
	}
}

func TestResetAwareSelector_RespectsPriorityTiers(t *testing.T) {
	t.Parallel()

	low := resetAwareAuth("low", 0.5, 10*time.Hour, 0)
	low.Attributes = map[string]string{"priority": "0"}
	high := resetAwareAuth("high", 0.1, 160*time.Hour, 0)
	high.Attributes = map[string]string{"priority": "5"}

	shares := resetAwareShares(t, []*Auth{low, high}, 500)
	if shares["high"] != 1 {
		t.Fatalf("shares = %v, want every selection in the highest priority tier", shares)
	}
}
