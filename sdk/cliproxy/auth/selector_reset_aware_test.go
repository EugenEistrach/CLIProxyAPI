package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var (
	berlin        = time.FixedZone("CET", 3600)
	resetAwareNow = time.Date(2026, 11, 3, 12, 41, 0, 0, berlin)
	appReserve    = ResetAwareReserve{Account: "eugen", Share: 0.2, ReleaseBefore: 12 * time.Hour}
)

func resetAwareSignals(weekly float64, weeklyResetsAt time.Time, fiveHour float64, now time.Time) QuotaState {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weekly, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weeklyResetsAt.Unix(), 10),
	}
	if fiveHour >= 0 {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = strconv.FormatFloat(fiveHour, 'f', -1, 64)
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)
	}
	return QuotaState{ObservedAt: now.Add(-time.Minute), Signals: signals}
}

func resetAwareAuth(id string, weekly float64, weeklyResetIn time.Duration, fiveHour float64) *Auth {
	return &Auth{ID: id, Label: id, Quota: resetAwareSignals(weekly, resetAwareNow.Add(weeklyResetIn), fiveHour, resetAwareNow)}
}

func resetAwarePickWith(t *testing.T, selector *ResetAwareSelector, auths ...*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func resetAwarePick(t *testing.T, auths ...*Auth) string {
	t.Helper()
	return resetAwarePickWith(t, &ResetAwareSelector{now: func() time.Time { return resetAwareNow }}, auths...)
}

func todaysSubscriptions() []*Auth {
	return []*Auth{
		{ID: "eugen", Label: "eugen", Quota: resetAwareSignals(0.80, time.Date(2026, 11, 7, 5, 0, 0, 0, berlin), -1, resetAwareNow)},
		{ID: "inspi", Label: "inspi", Quota: resetAwareSignals(0.01, time.Date(2026, 11, 7, 22, 0, 0, 0, berlin), -1, resetAwareNow)},
		{ID: "apple", Label: "apple", Quota: resetAwareSignals(0.00, time.Date(2026, 11, 9, 3, 0, 0, 0, berlin), -1, resetAwareNow)},
	}
}

func TestResetAwareSelector_TodaysNumbersGoToTheSubscriptionFurthestBehindItsPace(t *testing.T) {
	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }}
	if got := resetAwarePickWith(t, selector, todaysSubscriptions()...); got != "inspi" {
		t.Fatalf("picked %q, want inspi: 37%% of its week has passed and it used 1%%, while eugen is 32 points ahead of its pace", got)
	}
}

func TestResetAwareSelector_TheAppReserveHoldsUntilShortlyBeforeItsReset(t *testing.T) {
	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }, Reserve: appReserve}
	eugen := resetAwareAuth("eugen", 0.80, 40*time.Hour, -1)
	others := resetAwareAuth("others", 0.99, 100*time.Hour, -1)

	if !selector.HoldsBack(eugen) {
		t.Fatal("eugen at 80% with 40h to its reset must be held back for the app")
	}
	if got := resetAwarePickWith(t, selector, eugen, resetAwareAuth("fresh", 0.0, 160*time.Hour, -1)); got != "fresh" {
		t.Fatalf("picked %q, want fresh while eugen's reserve holds", got)
	}

	released := resetAwareAuth("eugen", 0.80, 11*time.Hour, -1)
	if selector.HoldsBack(released) {
		t.Fatal("eugen 11h before its reset must be released, or its last 19% are lost")
	}
	if got := resetAwarePickWith(t, selector, released, others); got != "eugen" {
		t.Fatalf("picked %q, want eugen once its reserve is released", got)
	}
}

func TestResetAwareSelector_ASubscriptionAtItsLimitIsSkipped(t *testing.T) {
	full := resetAwareAuth("full", 0.99, 3*time.Hour, -1)
	later := resetAwareAuth("later", 0.5, 100*time.Hour, -1)
	if got := resetAwarePick(t, full, later); got != "later" {
		t.Fatalf("picked %q, want later once full is at the weekly ceiling", got)
	}

	busy := resetAwareAuth("busy", 0.1, 20*time.Hour, 0.92)
	if got := resetAwarePick(t, busy, later); got != "later" {
		t.Fatalf("picked %q, want later while busy's five-hour window is nearly full", got)
	}
}

func TestResetAwareSelector_AnUnobservedCredentialCountsAsAFreshWeek(t *testing.T) {
	observed := resetAwareAuth("observed", 0.3, 30*time.Hour, -1)
	unobserved := &Auth{ID: "unobserved", Label: "unobserved"}
	if got := resetAwarePick(t, unobserved, observed); got != "observed" {
		t.Fatalf("picked %q, want observed: it is behind its pace, a fresh week is not", got)
	}
}

func TestResetAwareSelector_AWindowPastItsResetCountsAsAFreshWeek(t *testing.T) {
	justReset := resetAwareAuth("just-reset", 0.2, -time.Hour, -1)
	midWeek := resetAwareAuth("mid-week", 0.3, 80*time.Hour, -1)
	if got := resetAwarePick(t, justReset, midWeek); got != "mid-week" {
		t.Fatalf("picked %q, want mid-week: just-reset now has a full week ahead", got)
	}
}

func TestResetAwareSelector_EveryCredentialAtALimitFallsBackToTheMostRoom(t *testing.T) {
	a := resetAwareAuth("a", 0.995, 10*time.Hour, -1)
	b := resetAwareAuth("b", 0.4, 90*time.Hour, 0.95)
	if got := resetAwarePick(t, a, b); got != "b" {
		t.Fatalf("picked %q, want b, the most room under its cap", got)
	}
}

func TestResetAwareSelector_RespectsPriorityTiers(t *testing.T) {
	low := resetAwareAuth("low", 0.0, 10*time.Hour, -1)
	low.Attributes = map[string]string{"priority": "0"}
	high := resetAwareAuth("high", 0.1, 160*time.Hour, -1)
	high.Attributes = map[string]string{"priority": "5"}
	if got := resetAwarePick(t, low, high); got != "high" {
		t.Fatalf("picked %q, want the highest priority tier", got)
	}
}

func TestResetAwareSelector_RecordsEveryCandidateAndWhy(t *testing.T) {
	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }, Reserve: appReserve}
	resetAwarePickWith(t, selector, todaysSubscriptions()...)

	decision := RecentResetAwareDecisions()[0]
	if decision.ChosenID != "inspi" || decision.Rule != "furthest behind weekly pace" {
		t.Fatalf("decision = %+v", decision)
	}
	var order, notes []string
	for _, c := range decision.Candidates {
		order = append(order, c.AuthID)
		notes = append(notes, c.Note)
	}
	if strings.Join(order, ",") != "inspi,apple,eugen" {
		t.Fatalf("order = %v, want eligible by how far behind pace, then the held back", order)
	}
	if notes[0] != "chosen" || !strings.HasPrefix(notes[1], "waits: less behind pace than inspi") || !strings.HasPrefix(notes[2], "reserve held") {
		t.Fatalf("notes = %q", notes)
	}

	line := resetAwareDecisionLine(decision, resetAwareNow)
	for _, want := range []string{"reset-aware pick: inspi (furthest behind weekly pace)", "inspi 7d=1% pace 37% behind +36", "eugen 7d=80% pace 47% behind -42 cap 80% (reserve)", "5h=?"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %q", line, want)
		}
	}
}

func TestSessionAffinity_AConversationLeavesASubscriptionThatReachedItsLimit(t *testing.T) {
	now := resetAwareNow
	resetAware := &ResetAwareSelector{now: func() time.Time { return now }, Reserve: appReserve}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: resetAware, TTL: time.Hour})
	defer selector.Stop()
	eugen := &Auth{ID: "eugen", Label: "eugen", Quota: resetAwareSignals(0.10, now.Add(100*time.Hour), -1, now)}
	other := &Auth{ID: "other", Label: "other", Quota: resetAwareSignals(0.0, now.Add(160*time.Hour), -1, now)}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`)}

	pick := func() string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{eugen, other})
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return got.ID
	}
	if got := pick(); got != "eugen" {
		t.Fatalf("first pick = %q, want eugen (furthest behind pace)", got)
	}
	eugen.Quota = resetAwareSignals(0.5, now.Add(100*time.Hour), -1, now)
	if got := pick(); got != "eugen" {
		t.Fatalf("pick = %q, want the conversation to stay on eugen while it is only ahead of pace", got)
	}
	eugen.Quota = resetAwareSignals(0.81, now.Add(100*time.Hour), -1, now)
	if got := pick(); got != "other" {
		t.Fatalf("pick = %q, want the conversation to leave eugen once it reaches the app reserve", got)
	}
}

type simulatedSubscription struct {
	auth     *Auth
	used     float64
	resetsAt time.Time
}

type simulation struct {
	subscriptions []*simulatedSubscription
	now           time.Time
	selector      *ResetAwareSelector
	leftAtReset   map[string][]float64
	reservedPicks int
}

func newSimulation(reserve ResetAwareReserve) *simulation {
	sim := &simulation{now: resetAwareNow, leftAtReset: map[string][]float64{}}
	sim.selector = &ResetAwareSelector{now: func() time.Time { return sim.now }, Reserve: reserve}
	for _, auth := range todaysSubscriptions() {
		used, _ := strconv.ParseFloat(auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"], 64)
		resetSeconds, _ := strconv.ParseInt(auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"], 10, 64)
		sim.subscriptions = append(sim.subscriptions, &simulatedSubscription{auth: auth, used: used, resetsAt: time.Unix(resetSeconds, 0)})
	}
	return sim
}

// run advances the clock in steps; each step this proxy's demand (in weekly allowances per hour)
// goes to the picked subscription, and the app uses its own account outside the proxy.
func (sim *simulation) run(t *testing.T, until time.Time, proxyPerHour, appPerHour float64, observe func()) {
	t.Helper()
	const step = 10 * time.Minute
	for ; sim.now.Before(until); sim.now = sim.now.Add(step) {
		for _, sub := range sim.subscriptions {
			if !sim.now.Before(sub.resetsAt) {
				sim.leftAtReset[sub.auth.ID] = append(sim.leftAtReset[sub.auth.ID], 1-sub.used)
				sub.used = 0
				sub.resetsAt = sub.resetsAt.Add(resetAwareWeeklyWindow)
			}
			sub.auth.Quota = resetAwareSignals(sub.used, sub.resetsAt, -1, sim.now)
		}
		auths := make([]*Auth, 0, len(sim.subscriptions))
		for _, sub := range sim.subscriptions {
			auths = append(auths, sub.auth)
		}
		chosenID := resetAwarePickWith(t, sim.selector, auths...)
		for _, sub := range sim.subscriptions {
			if sub.auth.ID == chosenID {
				if sim.selector.HoldsBack(sub.auth) {
					sim.reservedPicks++
				}
				sub.used = min(1, sub.used+proxyPerHour*step.Hours())
			}
			if sub.auth.ID == "eugen" {
				sub.used = min(1, sub.used+appPerHour*step.Hours())
			}
		}
		if observe != nil {
			observe()
		}
	}
}

func (sim *simulation) used(id string) float64 {
	for _, sub := range sim.subscriptions {
		if sub.auth.ID == id {
			return sub.used
		}
	}
	return -1
}

func (sim *simulation) table() string {
	var b strings.Builder
	for _, sub := range sim.subscriptions {
		fmt.Fprintf(&b, " %s=%.0f%%", sub.auth.ID, sub.used*100)
	}
	return b.String()
}

func TestResetAwareSimulation_ThisWeekAtThisWeeksPaceKeepsTheAppReserveAndBalancesTheRest(t *testing.T) {
	sim := newSimulation(appReserve)
	day := resetAwareNow.Truncate(24 * time.Hour)
	friday := time.Date(2026, 11, 6, 12, 0, 0, 0, berlin)
	var eugenOnFriday float64
	sim.run(t, time.Date(2026, 11, 7, 4, 59, 0, 0, berlin), 0.016, 0, func() {
		if sim.now.Equal(friday) {
			eugenOnFriday = sim.used("eugen")
		}
		if sim.now.Sub(day) >= 24*time.Hour {
			day = sim.now
			t.Logf("%s%s", sim.now.Format("Mon 02 15:04"), sim.table())
		}
	})

	if sim.reservedPicks != 0 {
		t.Fatalf("the proxy used eugen %d times while its reserve held", sim.reservedPicks)
	}
	if eugenOnFriday > 0.80 {
		t.Fatalf("eugen on Friday noon at %.0f%%: the proxy took the app's reserve", eugenOnFriday*100)
	}
	inspi, apple := sim.used("inspi"), sim.used("apple")
	if inspi < 0.35 || apple < 0.25 {
		t.Fatalf("inspi %.0f%%, apple %.0f%% before eugen's reset: the proxy's demand wasn't spread to the subscriptions behind their pace", inspi*100, apple*100)
	}
}

func TestResetAwareSimulation_HeavyUseLeavesNothingAtAnyReset(t *testing.T) {
	sim := newSimulation(ResetAwareReserve{})
	sim.run(t, resetAwareNow.Add(3*resetAwareWeeklyWindow), 3.0/168*1.1, 0, nil)

	for id, lefts := range sim.leftAtReset {
		for i, left := range lefts {
			allowed := 0.015
			if i == 0 {
				allowed = 0.03
			}
			if left > allowed {
				t.Fatalf("%s reset #%d with %.1f%% left (all: %v) although demand exceeds what the subscriptions serve; the first reset may lose up to 3%% while today's lopsided start evens out", id, i+1, left*100, lefts)
			}
		}
	}
}

func TestResetAwareSimulation_LightUseSpreadsWhatIsLeftInsteadOfDrainingOne(t *testing.T) {
	sim := newSimulation(ResetAwareReserve{})
	sim.run(t, resetAwareNow.Add(2*resetAwareWeeklyWindow), 0.010, 0, nil)
	start := sim.now
	sim.run(t, start.Add(resetAwareWeeklyWindow), 0.010, 0, nil)

	for id, lefts := range sim.leftAtReset {
		last := lefts[len(lefts)-1]
		if last < 0.35 || last > 0.65 {
			t.Fatalf("%s reset with %.0f%% left (all: %v): at about half of capacity each subscription should end its week about half used", id, last*100, lefts)
		}
	}
}

func TestClaudeUsageSignals_ConvertTheUsageEndpointIntoUnifiedSignals(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":29.0,"resets_at":"2026-10-31T11:00:00.229384+00:00"},
		"seven_day":{"utilization":44.0,"resets_at":"2026-11-02T01:59:59.887150+00:00"}}`)

	signals, err := claudeUsageSignals(body)
	if err != nil {
		t.Fatalf("claudeUsageSignals() error = %v", err)
	}
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.29",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1793444400",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.44",
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1793584799",
	}
	for key, value := range want {
		if signals[key] != value {
			t.Fatalf("signals[%s] = %q, want %q (all: %v)", key, signals[key], value, signals)
		}
	}

	unusedFiveHour, err := claudeUsageSignals([]byte(`{"five_hour":{"utilization":0.0,"resets_at":null},"seven_day":{"utilization":100.0,"resets_at":"2026-10-31T20:59:59Z"}}`))
	if err != nil || unusedFiveHour["Anthropic-Ratelimit-Unified-5h-Reset"] != "" || unusedFiveHour["Anthropic-Ratelimit-Unified-7d-Utilization"] != "1" {
		t.Fatalf("unused five-hour window: %v, %v", unusedFiveHour, err)
	}

	if _, err := claudeUsageSignals([]byte(`{"five_hour":null,"seven_day":null}`)); err == nil {
		t.Fatal("want an error without a weekly window")
	}
}

func TestApplyClaudeUsage_TheSelectorSeesPolledNumbers(t *testing.T) {
	selector := &ResetAwareSelector{now: func() time.Time { return resetAwareNow }}
	manager := NewManager(nil, selector, nil)
	if _, err := manager.Register(context.Background(), &Auth{ID: "polled", Provider: "claude", Label: "polled"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.applyClaudeUsage("polled", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.44",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(resetAwareNow.Add(43*time.Hour).Unix(), 10),
	}, resetAwareNow)

	auth, _ := manager.GetByID("polled")
	candidate := selector.candidateOf(auth, resetAwareNow)
	if !candidate.WeeklyObserved || candidate.WeeklyUtilization != 0.44 || candidate.WeeklyResetsAt.Sub(resetAwareNow) != 43*time.Hour {
		t.Fatalf("candidate = %+v", candidate)
	}
}
