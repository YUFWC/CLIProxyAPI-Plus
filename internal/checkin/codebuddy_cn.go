package checkin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddycn"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codeBuddyAdapter struct{}

func NewCodeBuddyAdapter() Adapter        { return codeBuddyAdapter{} }
func (codeBuddyAdapter) Provider() string { return "codebuddy-cn" }
func (codeBuddyAdapter) Identity(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	creds := codebuddycn.CheckinCredentialsFromMetadata(auth.Metadata, auth.Attributes)
	if creds.UID == "" {
		return ""
	}
	identity, _ := json.Marshal([]string{creds.UID, creds.EnterpriseID})
	return string(identity)
}
func (codeBuddyAdapter) RequiredMetadata() []string {
	return []string{"uid", "enterprise_id", "enterpriseId", "domain", "base_url", "account.uid", "account.enterpriseId", "auth.domain"}
}
func (codeBuddyAdapter) CheckIn(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, headers http.Header, _ string) Result {
	if auth == nil || auth.Provider != "codebuddy-cn" {
		return Result{Outcome: OutcomeBlocked, Message: "codebuddy-cn: account unavailable"}
	}
	creds := codebuddycn.CheckinCredentialsFromMetadata(auth.Metadata, auth.Attributes)
	if creds.AccessToken == "" {
		return Result{Outcome: OutcomeBlocked, Message: "credential_unavailable"}
	}
	if creds.UID == "" && strings.TrimSpace(headers.Get("X-User-Id")) == "" {
		return Result{Outcome: OutcomeBlocked, Message: "codebuddy_user_id_missing"}
	}
	client := codebuddycn.NewCheckinClient(helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0))
	result, err := client.CheckIn(ctx, creds, headers)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Message: err.Error()}
	}
	if result.AlreadyDone {
		return Result{Outcome: OutcomeAlreadyDone, Message: "Today's check-in is confirmed"}
	}
	return Result{Outcome: OutcomeSuccess, Message: "Check-in succeeded"}
}
