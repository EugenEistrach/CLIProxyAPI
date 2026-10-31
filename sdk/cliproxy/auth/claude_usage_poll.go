package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Claude subscriptions report their own five-hour and weekly usage at the OAuth usage endpoint
// (what Claude Code's /usage shows). Response headers only update a credential while it serves
// requests, are lost on restart, and miss usage from outside this proxy (the Claude apps on the
// same subscription), so the reset-aware selector would decide on stale or missing numbers. The
// poller reads the endpoint for every Claude OAuth credential at startup and then periodically,
// and writes the result into the same unified signals the response headers fill.
const (
	claudeUsageURL          = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageBeta         = "oauth-2025-04-20"
	ClaudeUsagePollInterval = 5 * time.Minute
)

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

// claudeUsageSignals converts the endpoint's percentages and timestamps into unified signals.
// A window without a reset time (unused so far) keeps its utilization and gets no reset signal.
func claudeUsageSignals(body []byte) (map[string]string, error) {
	var usage claudeUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	signals := map[string]string{}
	for window, observed := range map[string]*claudeUsageWindow{"5h": usage.FiveHour, "7d": usage.SevenDay} {
		if observed == nil || observed.Utilization == nil {
			continue
		}
		utilization := math.Max(0, *observed.Utilization/100)
		signals[resetAwareSignalPrefix+window+"-Utilization"] = strconv.FormatFloat(utilization, 'f', -1, 64)
		if observed.ResetsAt == nil || strings.TrimSpace(*observed.ResetsAt) == "" {
			continue
		}
		resetsAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*observed.ResetsAt))
		if err != nil {
			return nil, fmt.Errorf("decode %s reset: %w", window, err)
		}
		signals[resetAwareSignalPrefix+window+"-Reset"] = strconv.FormatInt(resetsAt.Unix(), 10)
	}
	if signals[resetAwareSignalPrefix+"7d-Utilization"] == "" {
		return nil, fmt.Errorf("usage has no weekly window")
	}
	return signals, nil
}

func isClaudeSubscription(auth *Auth) bool {
	return auth != nil && !auth.Disabled && strings.EqualFold(auth.Provider, "claude") && auth.AuthKind() != AuthKindAPIKey
}

// StartClaudeUsagePolling reads every Claude subscription's usage now and then every interval
// until ctx ends.
func (m *Manager) StartClaudeUsagePolling(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = ClaudeUsagePollInterval
	}
	go func() {
		m.pollClaudeUsage(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.pollClaudeUsage(ctx)
			}
		}
	}()
}

func (m *Manager) pollClaudeUsage(ctx context.Context) {
	executor := m.executorFor("claude")
	if executor == nil {
		return
	}
	for _, auth := range m.List() {
		if !isClaudeSubscription(auth) {
			continue
		}
		signals, err := fetchClaudeUsage(ctx, executor, auth)
		if err != nil {
			log.Warnf("claude usage: %s: %v", resetAwareLabel(auth), err)
			continue
		}
		m.applyClaudeUsage(auth.ID, signals, time.Now())
	}
}

func fetchClaudeUsage(ctx context.Context, executor ProviderExecutor, auth *Auth) (map[string]string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("anthropic-beta", claudeUsageBeta)
	req.Header.Set("Accept", "application/json")
	resp, err := executor.HttpRequest(requestCtx, auth, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint answered %d", resp.StatusCode)
	}
	return claudeUsageSignals(body)
}

// applyClaudeUsage merges the polled windows into the credential's signals, keeping the other
// signals the last response recorded, and hands the updated credential to the scheduler.
func (m *Manager) applyClaudeUsage(authID string, signals map[string]string, now time.Time) {
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok {
		m.mu.Unlock()
		return
	}
	merged := make(map[string]string, len(auth.Quota.Signals)+len(signals))
	for key, value := range auth.Quota.Signals {
		merged[key] = value
	}
	for _, window := range []string{"5h", "7d"} {
		delete(merged, resetAwareSignalPrefix+window+"-Reset")
	}
	for key, value := range signals {
		merged[key] = value
	}
	auth.Quota.Signals = merged
	auth.Quota.ObservedAt = now
	snapshot := auth.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}
