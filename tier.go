package sdk

import (
	"strings"
	"sync"
	"sync/atomic"
)

// TierRank defines the precedence of license tiers. A higher rank means more
// privileges. Free is always the floor — a box that cannot verify its license
// (missing JWT, expired trial, cloud-confirmed revocation) keeps running with
// FREE-tier features instead of exiting.
//
// Trial licenses carry tir="pro" with an ExpiresAt. They behave like Pro until
// expiry; afterwards the SDK auto-downgrades the runtime tier to "free".
//
// Enterprise tier was retired (2026-04-30) — features collapsed into Pro.
var TierRank = map[string]int{
	"":      1, // missing/unknown — treat as free
	"free":  1,
	"basic": 2,
	"pro":   3,
}

// HasTier returns true when the box's current effective tier is at least
// `required`. The current tier is read from the SDK's runtime state, which is
// initialized by InitBox and may be downgraded later by trial-expiry or
// revocation watchdogs.
//
// Use this in HTTP handlers to gate features:
//
//	if !sdk.HasTier("pro") {
//	    http.Error(w, "pro tier required", http.StatusPaymentRequired)
//	    return
//	}
func HasTier(required string) bool {
	return TierRank[normalizeTier(currentTier())] >= TierRank[normalizeTier(required)]
}

// Tier returns the box's current effective tier. May differ from the original
// JWT claim if the trial has expired or the license has been revoked.
func Tier() string {
	return currentTier()
}

// IsLicenseExpired returns true if the SDK has downgraded the runtime tier to
// FREE due to trial expiry, cloud-confirmed revocation or instance-limit
// rejection. Boxes can show an upgrade banner / inform the user.
func IsLicenseExpired() bool {
	s := globalLicenseState()
	if s == nil {
		return false
	}
	return s.expired.Load()
}

func normalizeTier(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if _, ok := TierRank[t]; ok {
		return t
	}
	// Legacy values seen in old JWTs — collapse to nearest current tier.
	switch t {
	case "trial":
		// Trial JWTs should carry tir="pro" already, but be defensive.
		return "pro"
	case "enterprise":
		// Enterprise retired — grandfathered licenses behave as Pro.
		return "pro"
	case "paid":
		return "basic"
	}
	return "free"
}

// ── Runtime tier state ───────────────────────────────────────────────────────

type licenseState struct {
	mu      sync.RWMutex
	tier    string
	expired atomic.Bool
}

var (
	stateMu sync.RWMutex
	state   *licenseState
)

func globalLicenseState() *licenseState {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}

func setGlobalTier(initial string) *licenseState {
	s := &licenseState{tier: normalizeTier(initial)}
	stateMu.Lock()
	state = s
	stateMu.Unlock()
	return s
}

func currentTier() string {
	s := globalLicenseState()
	if s == nil {
		return "free"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tier
}

// downgradeToFree marks the license as expired and sets the runtime tier to
// "free". Idempotent — safe to call multiple times.
func (s *licenseState) downgradeToFree() {
	s.mu.Lock()
	s.tier = "free"
	s.mu.Unlock()
	s.expired.Store(true)
}
