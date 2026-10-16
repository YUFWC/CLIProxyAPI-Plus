package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type backgroundCredentialExecutor struct {
	countingRefreshExecutor
	entered   chan struct{}
	release   chan struct{}
	rotations atomic.Int32
}

func (e *backgroundCredentialExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.rotations.Add(1)
	if e.entered != nil {
		select {
		case e.entered <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	prepared := auth.Clone()
	prepared.Metadata["access_token"] = "rotated-test-token"
	prepared.Metadata["refresh_token"] = "rotated-test-refresh"
	prepared.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return prepared, nil
}

func TestBackgroundCredentialSharesAutomaticRefreshRotation(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &backgroundCredentialExecutor{countingRefreshExecutor: countingRefreshExecutor{id: "codearts"}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	auth, errRegister := manager.Register(t.Context(), &Auth{ID: "background-rotation", Provider: "codearts", Metadata: map[string]any{
		"access_token": "expired-test-token", "refresh_token": "old-test-refresh", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339), "refresh_interval_seconds": 300,
	}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	job := manager.markRefreshPending(nil, auth.ID, auth.RegistrationEpoch, time.Now())
	if job == nil {
		t.Fatal("automatic refresh could not reserve expired credential")
	}
	t.Cleanup(func() { manager.finishRefreshJob(job, time.Time{}, false) })
	first := make(chan error, 1)
	go func() {
		_, err := manager.PrepareCredentialForBackground(t.Context(), auth.ID, auth.RegistrationEpoch)
		first <- err
	}()
	<-executor.entered
	second := make(chan error, 1)
	go func() {
		_, err := manager.refreshAuthForRequestAtEpoch(t.Context(), auth.ID, "", auth.RegistrationEpoch, refreshIfDue)
		second <- err
	}()
	close(executor.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := executor.rotations.Load(); got != 1 {
		t.Fatalf("concurrent preparation rotated refresh credentials %d times; want once", got)
	}
	current, _ := manager.GetByID(auth.ID)
	if current.Metadata["refresh_token"] != "rotated-test-refresh" {
		t.Fatal("rotation was not retained by the credential manager")
	}
}

func TestBackgroundCredentialRejectsDisabledAndStaleRegistration(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &backgroundCredentialExecutor{countingRefreshExecutor: countingRefreshExecutor{id: "codearts"}}
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(t.Context(), &Auth{ID: "disabled-background", Provider: "codearts", Disabled: true, Metadata: map[string]any{
		"access_token": "expired-test-token", "refresh_token": "old-test-refresh", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, errPrepare := manager.PrepareCredentialForBackground(WithForceRefresh(t.Context()), auth.ID, auth.RegistrationEpoch); errPrepare == nil {
		t.Fatal("background preparation accepted disabled credential")
	}
	if _, errPrepare := manager.PrepareCredentialForBackground(t.Context(), auth.ID, auth.RegistrationEpoch+1); errPrepare == nil {
		t.Fatal("background preparation accepted replaced registration")
	}
	if executor.rotations.Load() != 0 {
		t.Fatal("blocked background preparation rotated credentials")
	}
}

func TestBackgroundCredentialDoesNotSendExpiredStaticCredential(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &backgroundCredentialExecutor{countingRefreshExecutor: countingRefreshExecutor{id: "codearts"}}
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(t.Context(), &Auth{ID: "expired-static", Provider: "codearts", Metadata: map[string]any{"access_key": "test-ak", "secret_key": "test-sk", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, errPrepare := manager.PrepareCredentialForBackground(t.Context(), auth.ID, auth.RegistrationEpoch); errPrepare == nil {
		t.Fatal("expired static credential was returned as usable")
	}
	if executor.rotations.Load() != 0 {
		t.Fatal("credential without refresh material invoked refresh")
	}
}
