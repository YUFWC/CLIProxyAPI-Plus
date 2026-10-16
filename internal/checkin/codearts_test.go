package checkin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	codeartsauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codearts"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codeArtsCheckinTransport func(*http.Request) (*http.Response, error)

func (f codeArtsCheckinTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCodeArtsAdapterOutcomesAndSafeDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name        string
		activity    string
		claimCode   int
		wantOutcome string
		wantWarning string
		wantSteps   string
	}{
		{name: "claim_confirm_warning", activity: `{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}`, wantOutcome: OutcomeSuccess, wantWarning: "codearts_confirmation_failed", wantSteps: codeartsauth.WelfareDeliveryPath + "," + codeartsauth.WelfareClaimPath + "," + codeartsauth.WelfareConfirmPath},
		{name: "already_done", activity: `{"type":"DAILY_CLAIM","status":"CONFIRMED","claimable":false}`, wantOutcome: OutcomeAlreadyDone, wantSteps: codeartsauth.WelfareDeliveryPath},
		{name: "unavailable", activity: `{"type":"DAILY_CLAIM","status":"LOCKED","claimable":false}`, wantOutcome: OutcomeUnavailable, wantSteps: codeartsauth.WelfareDeliveryPath},
		{name: "claim_rejected", activity: `{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}`, claimCode: http.StatusForbidden, wantOutcome: OutcomeFailed, wantSteps: codeartsauth.WelfareDeliveryPath + "," + codeartsauth.WelfareClaimPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			var steps []string
			transport := codeArtsCheckinTransport(func(request *http.Request) (*http.Response, error) {
				steps = append(steps, request.URL.Path)
				if request.Header.Get("UID") != "latest-user" || request.Header.Get("X-Security-Token") != "latest-st" || !strings.HasPrefix(request.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=latest-ak,") {
					t.Error("latest auth credentials or business header did not reach signed request")
				}
				status := http.StatusOK
				body := ""
				switch request.URL.Path {
				case codeartsauth.WelfareDeliveryPath:
					body = `{"code":0,"data":{"items":[` + test.activity + `]}}`
				case codeartsauth.WelfareClaimPath:
					body = `{"code":0,"data":{"userBenefitId":"benefit"}}`
					if test.claimCode != 0 {
						status, body = test.claimCode, `{"message":"secret-token-sensitive-body"}`
					}
				case codeartsauth.WelfareConfirmPath:
					status, body = http.StatusBadRequest, `{"message":"secret-token-sensitive-body"}`
				default:
					t.Errorf("unexpected endpoint %s", request.URL.Path)
					status = http.StatusInternalServerError
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			auth := &coreauth.Auth{
				Provider:   "codearts",
				Attributes: map[string]string{"api_key": "latest-ak", "secret_key": "latest-sk", "security_token": "latest-st"},
				Metadata:   map[string]any{"access_key": "old-ak", "secret_key": "old-sk", "security_token": "old-st"},
			}
			result := NewCodeArtsAdapter().CheckIn(ctx, nil, auth, http.Header{"UID": {"latest-user"}}, "2026-10-15")
			if result.Outcome != test.wantOutcome || result.Warning != test.wantWarning || result.Date != "" {
				t.Fatalf("result=%+v want outcome=%s warning=%s without inferred date", result, test.wantOutcome, test.wantWarning)
			}
			if strings.Join(steps, ",") != test.wantSteps {
				t.Fatalf("steps=%v want=%s", steps, test.wantSteps)
			}
			for _, text := range []string{result.Message, result.Warning} {
				if strings.Contains(text, "secret-token") || strings.Contains(text, "latest-ak") || strings.Contains(text, "sensitive-body") {
					t.Fatalf("diagnostic leaked upstream credentials/body: %q", text)
				}
			}
		})
	}
}

func TestCodeArtsAdapterBlocksIncompleteCredentialsAndCancellation(t *testing.T) {
	calls := 0
	transport := codeArtsCheckinTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		t.Error("blocked or canceled task sent an HTTP request")
		return nil, context.Canceled
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	adapter := NewCodeArtsAdapter()
	for _, auth := range []*coreauth.Auth{nil, {Metadata: map[string]any{"access_key": "AK"}}, {Metadata: map[string]any{"secret_key": "SK"}}} {
		if result := adapter.CheckIn(ctx, nil, auth, nil, "2026-10-15"); result.Outcome != OutcomeBlocked {
			t.Fatalf("missing credentials result=%+v", result)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	result := adapter.CheckIn(canceled, nil, &coreauth.Auth{Metadata: map[string]any{"access_key": "AK", "secret_key": "SK"}}, nil, "2026-10-15")
	if result.Outcome != OutcomeFailed || result.Message != "checkin_canceled" || calls != 0 {
		t.Fatalf("cancellation result=%+v calls=%d", result, calls)
	}
}
