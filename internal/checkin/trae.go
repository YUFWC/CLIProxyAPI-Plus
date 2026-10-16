package checkin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/trae"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type traeAdapter struct{}

func NewTraeAdapter() Adapter        { return traeAdapter{} }
func (traeAdapter) Provider() string { return "trae" }
func (traeAdapter) Identity(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	creds := trae.CheckinCredentialsFromMetadata(auth.Metadata, auth.Attributes)
	if creds.UID == "" {
		return ""
	}
	identity, _ := json.Marshal([]string{creds.UID, creds.EnterpriseID})
	return string(identity)
}
func (traeAdapter) RequiredMetadata() []string {
	return []string{"uid", "account.uid", "auth.uid", "enterprise_id", "enterpriseId", "account.enterpriseId", "auth.enterprise_id", "auth.enterpriseId", "device_id", "deviceId", "auth.device_id", "auth.deviceId", "device_brand", "deviceBrand", "auth.device_brand", "auth.deviceBrand", "device_type", "deviceType", "auth.device_type", "auth.deviceType", "os_version", "osVersion", "auth.os_version", "auth.osVersion", "app_version", "appVersion", "auth.app_version", "auth.appVersion"}
}
func (traeAdapter) CheckIn(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, headers http.Header, _ string) Result {
	if auth == nil || auth.Provider != "trae" {
		return Result{Outcome: OutcomeBlocked, Message: "trae: account unavailable"}
	}
	creds := trae.CheckinCredentialsFromMetadata(auth.Metadata, auth.Attributes)
	if creds.AccessToken == "" {
		return Result{Outcome: OutcomeBlocked, Message: "credential_unavailable"}
	}
	if _, err := trae.CheckinHeaders(creds, headers); err != nil {
		return Result{Outcome: OutcomeBlocked, Message: "trae_device_id_required"}
	}
	client := trae.NewCheckinClient(helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0))
	result, err := client.CheckIn(ctx, creds, headers)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Message: err.Error()}
	}
	if result.Unavailable {
		return Result{Outcome: OutcomeUnavailable, Message: "Check-in is not enabled for this account"}
	}
	if result.AlreadyDone {
		return Result{Outcome: OutcomeAlreadyDone, Message: "Today's check-in is confirmed", Date: result.Date}
	}
	return Result{Outcome: OutcomeSuccess, Message: "Check-in confirmed by server state or entitlement", Date: result.Date}
}
