package checkin

import (
	"context"
	"errors"
	"io"
	"net/http"

	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSchedulerCredentialBlockRecoversOnCredentialReplacement(t *testing.T) {
	service, source, _, _ := schedulerHarness(t, "2026-10-15T12:00:00+08:00", &testAdapter{}, "credential-repair")
	source.prepare = func(context.Context, string, uint64) (*coreauth.Auth, error) {
		return nil, errors.New("synthetic refresh failure")
	}
	service.reconcile()
	service.execute(dequeue(t, service))
	if status := onlyStatus(t, service, "2026-10-15"); status.Completed || status.Error != "credential_unavailable" {
		t.Fatalf("expected credential block: %+v", status)
	}
	account, _ := source.GetByID("credential-repair")
	account.Metadata["notes"] = "unrelated update"
	source.put(account)
	service.reconcile()
	if len(service.jobs) != 0 {
		t.Fatal("unrelated update retried unavailable credentials")
	}
	account.CredentialVersion++
	account.Metadata["access_token"] = "repaired credential"
	source.put(account)
	source.prepare = nil
	service.reconcile()
	service.execute(dequeue(t, service))
	if !onlyStatus(t, service, "2026-10-15").Completed {
		t.Fatal("repaired credential did not resume its blocked daily checkin")
	}
	account.CredentialVersion++
	source.put(account)
	service.reconcile()
	if len(service.jobs) != 0 {
		t.Fatal("later token rotation repeated a completed checkin")
	}
}

func TestSchedulerResumesRealAdapterAfterMissingTokenIsRepaired(t *testing.T) {
	requests := 0
	ctx := buddyCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer repaired-test-token" || r.Header.Get("X-User-Id") != "user-a" {
			t.Error("repaired account identity did not reach upstream")
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"today_checked_in":true}}`)
	})
	auth := &coreauth.Auth{ID: "missing-token", Provider: "codebuddy-cn", RegistrationEpoch: 1, CredentialVersion: 1, Metadata: map[string]any{"uid": "user-a"}}
	source := &testSource{accounts: map[string]*coreauth.Auth{auth.ID: auth}}
	clock := &testClock{now: testTime("2026-10-15T12:00:00+08:00")}
	service := newService(source, clock, func(n int64) int64 { return n / 2 }, NewCodeBuddyAdapter())
	service.ctx = ctx
	service.ApplyConfig(&config.Config{AutoCheckin: config.AutoCheckinConfig{Providers: map[string]config.CheckinProviderConfig{"codebuddy-cn": {Enabled: true}}}})
	service.reconcile()
	service.execute(dequeue(t, service))
	if status := onlyStatus(t, service, "2026-10-15"); status.Error != "credential_unavailable" || status.Completed || requests != 0 {
		t.Fatalf("missing credential was not blocked before sending: %+v requests=%d", status, requests)
	}
	auth.Metadata["access_token"] = "repaired-test-token"
	auth.CredentialVersion++
	source.put(auth)
	service.reconcile()
	service.execute(dequeue(t, service))
	if status := onlyStatus(t, service, "2026-10-15"); !status.Completed || status.Outcome != OutcomeAlreadyDone || requests != 1 {
		t.Fatalf("repaired credential remained blocked: %+v requests=%d", status, requests)
	}
}
