package checkin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type testClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *testTimer
}
type testTimer struct {
	channel  chan time.Time
	duration time.Duration
}

func (clock *testClock) Now() time.Time      { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now }
func (clock *testClock) set(value time.Time) { clock.mu.Lock(); clock.now = value; clock.mu.Unlock() }
func (clock *testClock) NewTimer(duration time.Duration) schedulerTimer {
	timer := &testTimer{channel: make(chan time.Time, 1), duration: duration}
	if clock.timers != nil {
		clock.timers <- timer
	}
	return timer
}
func (timer *testTimer) Chan() <-chan time.Time { return timer.channel }
func (timer *testTimer) Stop() bool             { return true }

type testSource struct {
	mu       sync.Mutex
	accounts map[string]*coreauth.Auth
	prepare  func(context.Context, string, uint64) (*coreauth.Auth, error)
}

func (source *testSource) put(auth *coreauth.Auth) {
	source.mu.Lock()
	source.accounts[auth.ID] = auth.Clone()
	source.mu.Unlock()
}
func (source *testSource) remove(id string) {
	source.mu.Lock()
	delete(source.accounts, id)
	source.mu.Unlock()
}
func (source *testSource) List() []*coreauth.Auth {
	source.mu.Lock()
	defer source.mu.Unlock()
	ids := make([]string, 0, len(source.accounts))
	for id := range source.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]*coreauth.Auth, 0, len(ids))
	for _, id := range ids {
		result = append(result, source.accounts[id].Clone())
	}
	return result
}
func (source *testSource) GetByID(id string) (*coreauth.Auth, bool) {
	source.mu.Lock()
	defer source.mu.Unlock()
	auth, ok := source.accounts[id]
	return auth.Clone(), ok
}
func (source *testSource) PrepareCredentialForBackground(ctx context.Context, id string, epoch uint64) (*coreauth.Auth, error) {
	if source.prepare != nil {
		return source.prepare(ctx, id, epoch)
	}
	auth, ok := source.GetByID(id)
	if !ok || auth.RegistrationEpoch != epoch {
		return nil, errors.New("test_removed")
	}
	return auth, nil
}

type testAdapter struct {
	provider string
	call     func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result
}

func (adapter *testAdapter) Provider() string {
	if adapter.provider != "" {
		return adapter.provider
	}
	return "codearts"
}
func (*testAdapter) Identity(auth *coreauth.Auth) string {
	value, _ := auth.Metadata["subject"].(string)
	return value
}
func (*testAdapter) RequiredMetadata() []string { return []string{"device"} }
func (adapter *testAdapter) CheckIn(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, headers http.Header, date string) Result {
	if adapter.call != nil {
		return adapter.call(ctx, cfg, auth, headers, date)
	}
	return Result{Outcome: OutcomeSuccess}
}
func testAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "codearts", RegistrationEpoch: 1, Metadata: map[string]any{"subject": id, "access_token": "old"}}
}
func testConfig() *config.Config {
	return &config.Config{AutoCheckin: config.AutoCheckinConfig{Providers: map[string]config.CheckinProviderConfig{"codearts": {Enabled: true, Timezone: "Asia/Shanghai", Window: config.CheckinWindow{Start: "00:10", End: "03:10"}}}}}
}
func testTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
func schedulerHarness(t *testing.T, value string, adapter *testAdapter, ids ...string) (*Service, *testSource, *testClock, *config.Config) {
	t.Helper()
	source := &testSource{accounts: make(map[string]*coreauth.Auth)}
	for _, id := range ids {
		source.put(testAuth(id))
	}
	clock := &testClock{now: testTime(value)}
	service := newService(source, clock, func(n int64) int64 { return n / 2 }, adapter)
	service.ctx = context.Background()
	cfg := testConfig()
	service.ApplyConfig(cfg)
	t.Cleanup(func() {
		service.mu.Lock()
		for _, job := range service.active {
			job.cancel()
		}
		service.mu.Unlock()
	})
	return service, source, clock, cfg
}
func dequeue(t *testing.T, service *Service) *scheduledJob {
	t.Helper()
	select {
	case job := <-service.jobs:
		return job
	default:
		t.Fatal("expected queued check-in")
		return nil
	}
}
func onlyStatus(t *testing.T, service *Service, date string) AccountStatus {
	t.Helper()
	for _, status := range service.Status("").Accounts {
		if status.Date == date {
			return status
		}
	}
	t.Fatalf("missing business date %s", date)
	return AccountStatus{}
}

func TestSchedulerRecoveryAndContinuousRollover(t *testing.T) {
	service, source, clock, _ := schedulerHarness(t, "2026-10-14T23:59:00+08:00", &testAdapter{}, "a", "b")
	draws := 0
	service.random = func(n int64) int64 { draws++; return n/2 + int64(draws) }
	service.reconcile()
	service.execute(dequeue(t, service))
	service.execute(dequeue(t, service))
	clock.set(testTime("2026-10-15T00:00:00+08:00"))
	service.reconcile()
	if draws != 2 {
		t.Fatalf("daily scheduling drew %d samples, want one per account", draws)
	}
	for _, status := range service.Status("").Accounts {
		if status.Date != "2026-10-15" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, status.ScheduledAt)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(testTime("2026-10-15T00:10:00+08:00")) || !at.Before(testTime("2026-10-15T03:10:00+08:00")) {
			t.Fatalf("rollover escaped full window: %s", status.ScheduledAt)
		}
	}
	before := service.Status("")
	auth, _ := source.GetByID("a")
	auth.Metadata["access_token"] = "rotated"
	source.put(auth)
	clock.set(testTime("2026-10-15T00:05:00+08:00"))
	service.reconcile()
	if len(service.jobs) != 0 || draws != 2 {
		t.Fatal("periodic scan or token rotation replaced existing daily plans")
	}
	after := service.Status("")
	for index := range before.Accounts {
		if before.Accounts[index].ScheduledAt != after.Accounts[index].ScheduledAt {
			t.Fatal("ordinary refresh changed schedule")
		}
	}
	clock.set(testTime("2026-10-15T04:00:00+08:00"))
	service.reconcile()
	if len(service.jobs) != 2 {
		t.Fatal("missed execution times were not recovered after the window")
	}
}

func TestSchedulerStartupInsideAndOutsideWindow(t *testing.T) {
	service, _, _, _ := schedulerHarness(t, "2026-10-15T01:00:00+08:00", &testAdapter{}, "a")
	service.reconcile()
	status := onlyStatus(t, service, "2026-10-15")
	if status.ScheduledAt != "2026-10-15T02:05:00+08:00" || status.Phase != "scheduled" {
		t.Fatalf("startup must sample remaining window: %+v", status)
	}
	early, _, _, _ := schedulerHarness(t, "2026-10-15T00:05:00+08:00", &testAdapter{}, "a")
	early.reconcile()
	if len(early.jobs) != 1 {
		t.Fatal("startup before the window must recover immediately")
	}
	boundary, _, _, _ := schedulerHarness(t, "2026-10-15T03:10:00+08:00", &testAdapter{}, "a")
	boundary.reconcile()
	if len(boundary.jobs) != 1 {
		t.Fatal("end boundary must be outside the half-open window")
	}
}

func TestSchedulerBackpressureKeepsDuePlansAndDeduplicates(t *testing.T) {
	service, source, _, _ := schedulerHarness(t, "2026-10-15T12:00:00+08:00", &testAdapter{})
	for index := range queueCapacity + 9 {
		source.put(testAuth(fmt.Sprintf("account-%03d", index)))
	}
	service.reconcile()
	service.reconcile()
	if len(service.jobs) != queueCapacity {
		t.Fatalf("queue size %d exceeds bound or duplicate wake lost work", len(service.jobs))
	}
	queued, waiting := 0, 0
	for _, status := range service.Status("").Accounts {
		switch status.Phase {
		case "queued":
			queued++
		case "scheduled":
			waiting++
		}
	}
	if queued != queueCapacity || waiting != 9 {
		t.Fatalf("backpressure lost due plans: queued=%d waiting=%d", queued, waiting)
	}
	service.execute(dequeue(t, service))
	service.reconcile()
	if len(service.jobs) != queueCapacity {
		t.Fatal("a released queue slot did not admit a retained due plan")
	}
}

func TestSchedulerUsesRefreshedHeadersAndPreservesCompletedDay(t *testing.T) {
	calls := 0
	adapter := &testAdapter{call: func(_ context.Context, _ *config.Config, auth *coreauth.Auth, headers http.Header, _ string) Result {
		calls++
		if auth.Metadata["access_token"] != "fresh" || headers.Get("X-User") != "new-user" {
			t.Fatalf("execution did not use prepared metadata: token=%v header=%q", auth.Metadata["access_token"], headers.Get("X-User"))
		}
		return Result{Outcome: OutcomeAlreadyDone}
	}}
	service, source, _, cfg := schedulerHarness(t, "2026-10-15T12:00:00+08:00", adapter, "a")
	provider := cfg.AutoCheckin.Providers["codearts"]
	provider.Headers = map[string]string{"X-User": "${meta.uid}"}
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	source.prepare = func(_ context.Context, id string, _ uint64) (*coreauth.Auth, error) {
		auth, _ := source.GetByID(id)
		auth.Metadata["access_token"] = "fresh"
		auth.Metadata["meta"] = map[string]any{"uid": "new-user"}
		source.put(auth)
		return auth, nil
	}
	service.reconcile()
	service.execute(dequeue(t, service))
	status := onlyStatus(t, service, "2026-10-15")
	if !status.Completed || status.Outcome != OutcomeAlreadyDone {
		t.Fatalf("explicit upstream completion not retained: %+v", status)
	}
	auth, _ := source.GetByID("a")
	auth.Metadata["access_token"] = "later"
	source.put(auth)
	provider.Window.Start = "01:00"
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	service.reconcile()
	if calls != 1 || len(service.jobs) != 0 || !onlyStatus(t, service, "2026-10-15").Completed {
		t.Fatal("token or meaningful config change repeated a completed day")
	}
}

func TestSchedulerBlockedRepairIgnoresTokenAndUnrelatedMetadata(t *testing.T) {
	calls := 0
	adapter := &testAdapter{call: func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result {
		calls++
		return Result{Outcome: OutcomeBlocked, Message: "device_missing"}
	}}
	service, source, _, cfg := schedulerHarness(t, "2026-10-15T12:00:00+08:00", adapter, "a")
	service.reconcile()
	service.execute(dequeue(t, service))
	auth, _ := source.GetByID("a")
	auth.Metadata["access_token"] = "rotation"
	auth.Metadata["irrelevant"] = "new"
	source.put(auth)
	service.reconcile()
	service.reconcile()
	if len(service.jobs) != 0 {
		t.Fatal("ordinary metadata updates retried blocked task")
	}
	auth.Metadata["device"] = "repaired"
	source.put(auth)
	service.reconcile()
	service.execute(dequeue(t, service))
	if calls != 2 {
		t.Fatal("necessary metadata repair did not re-evaluate blocked task")
	}
	adapter.call = func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result {
		calls++
		return Result{Outcome: OutcomeFailed, Message: "network_failed"}
	}
	provider := cfg.AutoCheckin.Providers["codearts"]
	provider.Headers = map[string]string{"X-Test": "repair"}
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	service.reconcile()
	service.execute(dequeue(t, service))
	auth.Metadata["access_token"] = "another-rotation"
	source.put(auth)
	service.reconcile()
	if len(service.jobs) != 0 || onlyStatus(t, service, "2026-10-15").Completed {
		t.Fatal("failed attempt was retried or treated as complete")
	}
}

func TestSchedulerMidnightSerializesAccountsAndDoesNotInventResultDate(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &testAdapter{call: func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result {
		close(entered)
		<-release
		return Result{Outcome: OutcomeSuccess}
	}}
	service, _, clock, _ := schedulerHarness(t, "2026-10-15T23:59:00+08:00", adapter, "a")
	service.reconcile()
	finished := make(chan struct{})
	go func() { service.execute(dequeue(t, service)); close(finished) }()
	<-entered
	clock.set(testTime("2026-10-16T04:00:00+08:00"))
	service.reconcile()
	if len(service.jobs) != 0 {
		t.Fatal("new business day ran concurrently with old task")
	}
	close(release)
	<-finished
	yesterday := onlyStatus(t, service, "2026-10-15")
	if yesterday.Completed || yesterday.Error != "result_date_uncertain" {
		t.Fatalf("undated cross-midnight success falsely completed: %+v", yesterday)
	}
	service.reconcile()
	job := dequeue(t, service)
	if job.key.date != "2026-10-16" {
		t.Fatal("old result was incorrectly attributed to new day")
	}
}

func TestSchedulerRejectsStaleRegistrationAndSubject(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &testAdapter{call: func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result {
		close(entered)
		<-release
		return Result{Outcome: OutcomeSuccess}
	}}
	service, source, _, _ := schedulerHarness(t, "2026-10-15T12:00:00+08:00", adapter, "a")
	service.reconcile()
	job := dequeue(t, service)
	finished := make(chan struct{})
	go func() { service.execute(job); close(finished) }()
	<-entered
	replacement := testAuth("a")
	replacement.RegistrationEpoch = 2
	replacement.Metadata["subject"] = "replacement"
	source.put(replacement)
	service.reconcile()
	close(release)
	<-finished
	status := onlyStatus(t, service, "2026-10-15")
	if status.Completed || status.Phase != "scheduled" {
		t.Fatalf("stale result corrupted replacement plan: %+v", status)
	}
	service.reconcile()
	replacementJob := dequeue(t, service)
	if replacementJob.epoch != 2 {
		t.Fatal("replacement registration did not get its own task")
	}
	source.remove("a")
	service.reconcile()
	service.execute(replacementJob)
	if len(service.Status("").Accounts) != 0 {
		t.Fatal("removed credential retained future work")
	}
}

func TestSchedulerConfigIsEffectiveAndProviderScoped(t *testing.T) {
	service, _, _, cfg := schedulerHarness(t, "2026-10-15T01:00:00+08:00", &testAdapter{}, "a")
	service.reconcile()
	before := onlyStatus(t, service, "2026-10-15")
	same := cfg.CloneForRuntime()
	provider := same.AutoCheckin.Providers["codearts"]
	provider.Window.Start = "0:10"
	same.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(same)
	service.reconcile()
	if onlyStatus(t, service, "2026-10-15").ScheduledAt != before.ScheduledAt {
		t.Fatal("canonical-equivalent config reset schedule")
	}
	same.AutoCheckin.Providers["codearts"] = config.CheckinProviderConfig{Enabled: true, Timezone: "Asia/Shanghai", Window: config.CheckinWindow{Start: "02:00", End: "03:00"}}
	service.ApplyConfig(same)
	same.AutoCheckin.Providers["codearts"] = config.CheckinProviderConfig{}
	service.reconcile()
	if len(service.jobs) != 1 {
		t.Fatal("affected window change before start must recover immediately")
	}
	snapshot := service.Status("")
	if snapshot.Providers[0].ConfigFingerprint != Fingerprint(service.providers["codearts"].config) {
		t.Fatal("status does not identify active effective config")
	}
}

func TestSchedulerHomeAndDisabledAccountsNeverExecute(t *testing.T) {
	service, source, _, cfg := schedulerHarness(t, "2026-10-15T12:00:00+08:00", &testAdapter{}, "a")
	cfg.Home.Enabled = true
	service.ApplyConfig(cfg)
	service.reconcile()
	if len(service.jobs) != 0 || service.Status("").DisabledReason != "home_mode" {
		t.Fatal("home execution node accepted local check-in work")
	}
	cfg.Home.Enabled = false
	service.ApplyConfig(cfg)
	service.reconcile()
	job := dequeue(t, service)
	auth, _ := source.GetByID("a")
	auth.Disabled = true
	source.put(auth)
	service.reconcile()
	service.execute(job)
	if len(service.Status("").Accounts) != 0 {
		t.Fatal("disabled credential retained queued work")
	}
}

func TestSchedulerWorkersBoundConcurrencyAndStopWaitsForCancellation(t *testing.T) {
	entered := make(chan struct{}, workerCount)
	exited := make(chan struct{}, workerCount)
	adapter := &testAdapter{call: func(ctx context.Context, _ *config.Config, _ *coreauth.Auth, _ http.Header, _ string) Result {
		entered <- struct{}{}
		<-ctx.Done()
		exited <- struct{}{}
		return Result{Outcome: OutcomeFailed}
	}}
	source := &testSource{accounts: make(map[string]*coreauth.Auth)}
	for index := range workerCount + 3 {
		source.put(testAuth(fmt.Sprintf("a-%d", index)))
	}
	clock := &testClock{now: testTime("2026-10-15T12:00:00+08:00")}
	service := newService(source, clock, func(n int64) int64 { return n / 2 }, adapter)
	if err := service.Start(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Stop)
	for range workerCount {
		<-entered
	}
	select {
	case <-entered:
		t.Fatal("worker bound exceeded")
	default:
	}
	service.Stop()
	if len(exited) != workerCount {
		t.Fatal("Stop returned before active upstream requests exited")
	}
	for _, status := range service.Status("").Accounts {
		if status.Phase == "running" || status.Phase == "queued" {
			t.Fatal("Stop retained running or queued state")
		}
	}
}

func TestSchedulerSanitizesDiagnostics(t *testing.T) {
	adapter := &testAdapter{call: func(context.Context, *config.Config, *coreauth.Auth, http.Header, string) Result {
		return Result{Outcome: OutcomeFailed, Message: "Bearer secret-token /home/private/auth.json", Warning: "raw upstream body"}
	}}
	service, _, _, _ := schedulerHarness(t, "2026-10-15T12:00:00+08:00", adapter, "a")
	service.reconcile()
	service.execute(dequeue(t, service))
	status := onlyStatus(t, service, "2026-10-15")
	if status.Error != "checkin_failed" || status.Warning != "" || strings.Contains(status.Account, "private") {
		t.Fatalf("unsafe diagnostic escaped: %+v", status)
	}
}

func TestSchedulerConfigChangeLeavesOtherProviderPlanUntouched(t *testing.T) {
	source := &testSource{accounts: make(map[string]*coreauth.Auth)}
	source.put(testAuth("arts"))
	other := testAuth("trae")
	other.Provider = "trae"
	source.put(other)
	clock := &testClock{now: testTime("2026-10-15T01:00:00+08:00")}
	draws := 0
	service := newService(source, clock, func(n int64) int64 { draws++; return n / 2 }, &testAdapter{}, &testAdapter{provider: "trae"})
	service.ctx = context.Background()
	cfg := testConfig()
	cfg.AutoCheckin.Providers["trae"] = cfg.AutoCheckin.Providers["codearts"]
	service.ApplyConfig(cfg)
	service.reconcile()
	original := service.plans[planKey{"trae", "trae", "2026-10-15"}]
	changed := cfg.AutoCheckin.Providers["codearts"]
	changed.Window.End = "03:00"
	cfg.AutoCheckin.Providers["codearts"] = changed
	service.ApplyConfig(cfg)
	service.reconcile()
	if service.plans[planKey{"trae", "trae", "2026-10-15"}] != original || draws != 3 {
		t.Fatal("a provider-local change replanned another provider")
	}
}

func TestSchedulerSubjectReplacementCannotInheritCompletedDay(t *testing.T) {
	service, source, _, _ := schedulerHarness(t, "2026-10-15T12:00:00+08:00", &testAdapter{}, "a")
	service.reconcile()
	service.execute(dequeue(t, service))
	replacement, _ := source.GetByID("a")
	replacement.Metadata["subject"] = "different-user"
	source.put(replacement)
	service.reconcile()
	if len(service.jobs) != 1 || onlyStatus(t, service, "2026-10-15").Completed {
		t.Fatal("same-file replacement inherited another subject's completed day")
	}
}

func TestSchedulerExplicitDateEvidenceCanCompleteCrossMidnightTask(t *testing.T) {
	adapter := &testAdapter{}
	service, _, clock, _ := schedulerHarness(t, "2026-10-15T23:59:00+08:00", adapter, "a")
	adapter.call = func(_ context.Context, _ *config.Config, _ *coreauth.Auth, _ http.Header, date string) Result {
		clock.set(testTime("2026-10-16T00:01:00+08:00"))
		return Result{Outcome: OutcomeSuccess, Date: date}
	}
	service.reconcile()
	service.execute(dequeue(t, service))
	service.reconcile()
	if !onlyStatus(t, service, "2026-10-15").Completed || onlyStatus(t, service, "2026-10-16").Completed {
		t.Fatal("explicit business date did not retain ownership across midnight")
	}
	clock.set(testTime("2026-10-19T00:01:00+08:00"))
	service.reconcile()
	for _, status := range service.Status("").Accounts {
		if status.Date < "2026-10-18" {
			t.Fatal("obsolete daily state was not pruned")
		}
	}
}

func TestSchedulerCalibrationRecoversMissedAccountNotification(t *testing.T) {
	called := make(chan string, 1)
	adapter := &testAdapter{call: func(_ context.Context, _ *config.Config, auth *coreauth.Auth, _ http.Header, _ string) Result {
		called <- auth.ID
		return Result{Outcome: OutcomeSuccess}
	}}
	source := &testSource{accounts: make(map[string]*coreauth.Auth)}
	clock := &testClock{now: testTime("2026-10-15T12:00:00+08:00"), timers: make(chan *testTimer, 16)}
	service := newService(source, clock, func(n int64) int64 { return n / 2 }, adapter)
	if err := service.Start(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Stop)
	// Consume Start's configuration wake before selecting the active timer.
	<-clock.timers
	timer := <-clock.timers
	if timer.duration > clockCalibration {
		t.Fatal("calibration wake was not bounded")
	}
	source.put(testAuth("missed-notification"))
	clock.set(testTime("2026-10-15T12:00:30+08:00"))
	timer.channel <- clock.Now()
	if id := <-called; id != "missed-notification" {
		t.Fatal("periodic wake did not reconcile source accounts")
	}
}

func TestSchedulerEquivalentHeaderMapsDoNotResetPlans(t *testing.T) {
	service, _, _, cfg := schedulerHarness(t, "2026-10-15T01:00:00+08:00", &testAdapter{}, "a")
	service.reconcile()
	key := planKey{"codearts", "a", "2026-10-15"}
	original := service.plans[key]
	provider := cfg.AutoCheckin.Providers["codearts"]
	provider.Headers = map[string]string{}
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	service.reconcile()
	if service.plans[key] != original {
		t.Fatal("nil and empty headers changed effective scheduling state")
	}
	provider.Headers = map[string]string{"UID": "static-value"}
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	service.reconcile()
	original = service.plans[key]
	provider.Headers = map[string]string{"uid": "static-value"}
	cfg.AutoCheckin.Providers["codearts"] = provider
	service.ApplyConfig(cfg)
	service.reconcile()
	if service.plans[key] != original {
		t.Fatal("HTTP header case-only edit changed effective scheduling state")
	}
}
