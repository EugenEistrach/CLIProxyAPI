package auth

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetAwareSelector spreads new selections across subscription credentials in proportion to
// how urgently each one's weekly allowance has to be used: the weekly allowance left divided by
// the time until that window resets. Allowance left at a reset is lost, so the credential whose
// week ends soonest with allowance left gets the largest share, while every credential with
// headroom keeps a share instead of one credential carrying all traffic. A credential near the
// end of its five-hour or weekly window stops receiving new selections before it hits the limit.
//
// The usage comes from the Anthropic unified rate-limit headers the host already records per
// credential (QuotaState.Signals). A credential without observations counts as half used with a
// full window ahead, so it is tried and observed without being treated as empty or exhausted.
// Selection is a weighted random draw rather than smooth weighted round-robin, because the
// weights move with every observed response and smooth round-robin restarts its credits on
// every weight change. Session affinity keeps an established conversation on its credential;
// this selector only places new conversations and requests that must move.
type ResetAwareSelector struct {
	mu   sync.Mutex
	rand *rand.Rand
	now  func() time.Time
}

const (
	resetAwareSignalPrefix       = "Anthropic-Ratelimit-Unified-"
	resetAwareUnknownUtilization = 0.5
	resetAwareFiveHourCeiling    = 0.9
	resetAwareWeeklyCeiling      = 0.97
	resetAwareMinimumHorizon     = 6 * time.Hour
	resetAwareFiveHourWindow     = 5 * time.Hour
	resetAwareWeeklyWindow       = 7 * 24 * time.Hour
)

type resetAwareWindow struct {
	utilization float64
	resetsAt    time.Time
}

// observedResetAwareWindow reads one unified window ("5h" or "7d") from a credential's signals.
// A window whose reset has passed counts as fresh, since its usage no longer applies.
func observedResetAwareWindow(auth *Auth, window string, length time.Duration, now time.Time) resetAwareWindow {
	unknown := resetAwareWindow{utilization: resetAwareUnknownUtilization, resetsAt: now.Add(length)}
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return unknown
	}
	utilization, errUtilization := strconv.ParseFloat(strings.TrimSpace(auth.Quota.Signals[resetAwareSignalPrefix+window+"-Utilization"]), 64)
	resetSeconds, errReset := strconv.ParseInt(strings.TrimSpace(auth.Quota.Signals[resetAwareSignalPrefix+window+"-Reset"]), 10, 64)
	if errUtilization != nil || errReset != nil || utilization < 0 || resetSeconds <= 0 {
		return unknown
	}
	resetsAt := time.Unix(resetSeconds, 0)
	if !resetsAt.After(now) {
		return resetAwareWindow{utilization: 0, resetsAt: now.Add(length)}
	}
	return resetAwareWindow{utilization: utilization, resetsAt: resetsAt}
}

// resetAwareWeight is the credential's share of new selections; zero keeps it out of new ones.
func resetAwareWeight(auth *Auth, now time.Time) float64 {
	weekly := observedResetAwareWindow(auth, "7d", resetAwareWeeklyWindow, now)
	fiveHour := observedResetAwareWindow(auth, "5h", resetAwareFiveHourWindow, now)
	if weekly.utilization >= resetAwareWeeklyCeiling || fiveHour.utilization >= resetAwareFiveHourCeiling {
		return 0
	}
	horizon := weekly.resetsAt.Sub(now)
	if horizon < resetAwareMinimumHorizon {
		horizon = resetAwareMinimumHorizon
	}
	return (1 - weekly.utilization) / horizon.Hours() * (1 - fiveHour.utilization)
}

func (s *ResetAwareSelector) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *ResetAwareSelector) draw(limit float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rand == nil {
		return rand.Float64() * limit
	}
	return s.rand.Float64() * limit
}

// Pick draws among the available credentials by reset-aware weight. When every credential is
// near a limit, the one with the most weekly allowance left is used, so requests still go out
// and the host's own cooldowns handle an actual rejection.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.currentTime()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	weights := make([]float64, len(available))
	total := 0.0
	for i, auth := range available {
		weights[i] = resetAwareWeight(auth, now)
		total += weights[i]
	}
	if total <= 0 {
		roomiest := available[0]
		for _, auth := range available[1:] {
			if observedResetAwareWindow(auth, "7d", resetAwareWeeklyWindow, now).utilization <
				observedResetAwareWindow(roomiest, "7d", resetAwareWeeklyWindow, now).utilization {
				roomiest = auth
			}
		}
		return roomiest, nil
	}

	point := s.draw(total)
	for i, auth := range available {
		point -= weights[i]
		if point < 0 {
			return auth, nil
		}
	}
	return available[len(available)-1], nil
}
