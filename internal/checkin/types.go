// Package checkin runs provider daily rewards independently of inference traffic.
package checkin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	OutcomeSuccess     = "success"
	OutcomeAlreadyDone = "already_done"
	OutcomeUnavailable = "unavailable"
	OutcomeFailed      = "failed"
	OutcomeBlocked     = "blocked"
)

// Result contains only safe diagnostics, never upstream response bodies or secrets.
type Result struct {
	Outcome string
	Message string
	Warning string
	Date    string
}

// Adapter implements one provider's authenticated check-in protocol.
type Adapter interface {
	Provider() string
	CheckIn(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result
}

// AccountSource retains ownership of credential refresh and persistence.
type AccountSource interface {
	List() []*coreauth.Auth
	GetByID(string) (*coreauth.Auth, bool)
	PrepareCredentialForBackground(context.Context, string, uint64) (*coreauth.Auth, error)
}

// Capability describes an implemented provider without exposing credential material.
type Capability struct {
	ID               string                       `json:"id"`
	Name             string                       `json:"name"`
	Defaults         config.CheckinProviderConfig `json:"defaults"`
	ProtectedHeaders []string                     `json:"protected_headers"`
}

// AccountStatus is the public, in-memory status for one credential and business day.
type AccountStatus struct {
	AuthID      string `json:"auth_id"`
	Account     string `json:"account"`
	Provider    string `json:"provider"`
	Date        string `json:"date"`
	Phase       string `json:"phase"`
	Trigger     string `json:"trigger"`
	Outcome     string `json:"outcome,omitempty"`
	Completed   bool   `json:"completed"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Error       string `json:"error,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// ProviderStatus identifies the effective scheduling configuration.
type ProviderStatus struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	Timezone          string `json:"timezone"`
	Date              string `json:"date"`
	ConfigFingerprint string `json:"config_fingerprint"`
}

// Snapshot is safe to publish through the authenticated management API.
type Snapshot struct {
	ConfigFingerprint string           `json:"config_fingerprint"`
	DisabledReason    string           `json:"disabled_reason,omitempty"`
	Providers         []ProviderStatus `json:"providers"`
	Accounts          []AccountStatus  `json:"accounts"`
}

// Fingerprint identifies an effective provider configuration without returning its headers.
func Fingerprint(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
