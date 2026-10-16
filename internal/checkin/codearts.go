package checkin

import (
	"context"
	"net/http"
	"strings"

	codeartsauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codearts"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codeArtsAdapter struct{}

// NewCodeArtsAdapter reuses the signed CodeArts daily reward workflow.
func NewCodeArtsAdapter() Adapter { return codeArtsAdapter{} }

func (codeArtsAdapter) Provider() string { return codeartsauth.Provider }

func (codeArtsAdapter) Identity(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	userID, _ := auth.Metadata["user_id"].(string)
	domainID, _ := auth.Metadata["domain_id"].(string)
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	return strings.TrimSpace(domainID) + "\x00" + userID
}

func (codeArtsAdapter) RequiredMetadata() []string { return []string{"access_key", "secret_key"} }

func (codeArtsAdapter) CheckIn(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, headers http.Header, _ string) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	accessKey, secretKey, securityToken := helps.CodeArtsCredentialTriple(auth)
	if accessKey == "" || secretKey == "" {
		return Result{Outcome: OutcomeBlocked, Message: "credential_unavailable"}
	}
	if ctx.Err() != nil {
		return Result{Outcome: OutcomeFailed, Message: "checkin_canceled"}
	}
	client := codeartsauth.NewCheckInClient(helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0), headers)
	result, err := client.DailyCheckIn(ctx, codeartsauth.Credentials{
		AccessKey:     accessKey,
		SecretKey:     secretKey,
		SecurityToken: securityToken,
	})
	if err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: OutcomeFailed, Message: "checkin_canceled"}
		}
		// Protocol errors may contain upstream bodies or credentials. Never publish them.
		return Result{Outcome: OutcomeFailed, Message: "codearts_checkin_failed"}
	}
	if result == nil {
		return Result{Outcome: OutcomeFailed, Message: "codearts_result_invalid"}
	}
	out := Result{Outcome: OutcomeUnavailable, Message: "codearts_activity_unavailable"}
	if result.Claimed {
		out.Outcome = OutcomeSuccess
		out.Message = "codearts_checkin_succeeded"
		if result.ConfirmError != "" {
			out.Warning = "codearts_confirmation_failed"
		}
	} else if result.AlreadyClaimed {
		out.Outcome = OutcomeAlreadyDone
		out.Message = "codearts_already_done"
	}
	// These endpoints provide no explicit reward date. The scheduler owns the
	// requested business day and detects uncertain results across its day boundary.
	return out
}
