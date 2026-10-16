package codearts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDailyCheckInRequiresCompletedDailyStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		activity string
		already  bool
		fallback bool
	}{
		{name: "claimed", activity: `{"type":"DAILY_CLAIM","status":"CLAIMED","claimable":false}`, already: true},
		{name: "confirmed", activity: `{"type":"USER_LOGIN","status":"CONFIRMED","claimable":true}`, already: true},
		{name: "consumed", activity: `{"type":"DAILY_CLAIM","status":"CONSUMED","claimable":false}`, already: true},
		{name: "not_open", activity: `{"type":"DAILY_CLAIM","status":"ELIGIBLE","claimable":false}`},
		{name: "unknown", activity: `{"type":"DAILY_CLAIM","status":"LOCKED","claimable":false}`},
		{name: "missing_status", activity: `{"type":"DAILY_CLAIM","claimable":false}`},
		{name: "non_daily_completed", activity: `{"type":"STUDENT_CERTIFIED","status":"CONFIRMED","claimable":false}`, fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fallbackCalls := 0
			client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case WelfareDeliveryPath:
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[%s]}}`, test.activity)
				case BenefitClaimPath:
					fallbackCalls++
					_, _ = w.Write([]byte(`{"error_code":"NOT_AVAILABLE"}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			result, err := client.DailyCheckIn(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK"})
			if test.fallback {
				if err == nil || fallbackCalls != 1 {
					t.Fatalf("non-daily completion must not imply daily completion: result=%+v err=%v fallback=%d", result, err, fallbackCalls)
				}
				return
			}
			if err != nil || result == nil || result.Claimed || result.AlreadyClaimed != test.already || fallbackCalls != 0 {
				t.Fatalf("result=%+v err=%v fallback=%d; want already=%v", result, err, fallbackCalls, test.already)
			}
		})
	}
}

func TestDailyCheckInClaimFailureDoesNotFallback(t *testing.T) {
	fallbackCalls := 0
	client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case WelfareDeliveryPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}]}}`))
		case WelfareClaimPath:
			_, _ = w.Write([]byte(`{"code":403,"message":"claim rejected"}`))
		case BenefitClaimPath:
			fallbackCalls++
			_, _ = w.Write([]byte(`{"error_code":"0000"}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	result, err := client.DailyCheckIn(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK"})
	if err == nil || result != nil || fallbackCalls != 0 {
		t.Fatalf("claim failure must not become fallback success: result=%+v err=%v fallback=%d", result, err, fallbackCalls)
	}
}

func TestCheckInHeadersAreSignedOnEveryBusinessStep(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_%v", fallback), func(t *testing.T) {
			var steps []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				steps = append(steps, r.URL.Path)
				if r.Header.Get("UID") != "account-42" || r.Header.Get("X-Language") != "en-us" {
					t.Errorf("configured headers did not survive %s", r.URL.Path)
				}
				if r.Header.Get("Accept") != "" || r.Header.Get("Content-Type") != "" || r.Header.Get("Accept-Encoding") != "" {
					t.Errorf("defaults overwrote configured empty headers on %s", r.URL.Path)
				}
				if values := r.Header.Values("Accept-Encoding"); len(values) != 1 || values[0] != "" {
					t.Errorf("transport changed configured encoding values: %q", values)
				}
				if r.Header.Get("Session-Id") != "" || r.Header.Get("Client-Id") != "" {
					t.Error("inference headers leaked into check-in")
				}
				verifyCheckInWireSignature(t, r)
				switch r.URL.Path {
				case WelfareDeliveryPath:
					if fallback {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}]}}`))
				case WelfareClaimPath:
					_, _ = w.Write([]byte(`{"code":0,"data":{"userBenefitId":"benefit"}}`))
				case WelfareConfirmPath:
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error_code":"confirm_failed","error_msg":"sensitive-body"}`))
				case BenefitClaimPath:
					_, _ = w.Write([]byte(`{"error_code":"0000"}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			headers := http.Header{"UID": {"account-42"}, "x-language": {"en-us"}, "Accept": {""}, "Content-Type": {""}, "Accept-Encoding": {""}}
			client := NewCheckInClient(server.Client(), headers)
			client.snapHost, client.benefitURL = server.URL, server.URL
			headers["UID"][0] = "mutated"
			result, err := client.DailyCheckIn(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"})
			if err != nil || result == nil || !result.Claimed {
				t.Fatalf("check-in result=%+v err=%v", result, err)
			}
			want := WelfareDeliveryPath + "," + WelfareClaimPath + "," + WelfareConfirmPath
			if fallback {
				want = WelfareDeliveryPath + "," + BenefitClaimPath
			}
			if strings.Join(steps, ",") != want {
				t.Fatalf("steps=%v want=%s", steps, want)
			}
			if !fallback && result.ConfirmError == "" {
				t.Fatal("claim must stay successful with a confirm warning")
			}
		})
	}
}

func verifyCheckInWireSignature(t *testing.T, r *http.Request) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request: %v", err)
		return
	}
	authorization := r.Header.Get("Authorization")
	_, suffix, found := strings.Cut(authorization, "SignedHeaders=")
	if !found {
		t.Error("missing signed headers")
		return
	}
	names, _, _ := strings.Cut(suffix, ",")
	headers := map[string]string{}
	for _, name := range strings.Split(names, ";") {
		if name == "host" {
			headers[name] = r.Host
		} else if name == "x-sdk-date" {
			headers[SdkDateHeader] = r.Header.Get(name)
		} else {
			headers[name] = r.Header.Get(name)
		}
	}
	if !strings.Contains(";"+names+";", ";uid;") {
		t.Error("UID was appended after signing")
	}
	now, err := time.Parse(sdkDateFormat, r.Header.Get(SdkDateHeader))
	if err != nil {
		t.Errorf("invalid signing date: %v", err)
		return
	}
	signed := Sign(SignOptions{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(), Headers: headers, Body: body, Now: func() time.Time { return now }}, "AK", "SK")
	if signed["Authorization"] != authorization {
		t.Error("signature does not match exact wire headers and body")
	}
}

func TestCheckInClientRejectsRedirects(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer origin.Close()
	client := NewCheckInClient(origin.Client(), http.Header{"UID": {"private-account"}})
	client.snapHost, client.benefitURL = origin.URL, origin.URL
	result, err := client.DailyCheckIn(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK"})
	if err == nil || result != nil || targetCalls != 0 {
		t.Fatalf("redirect accepted: result=%+v err=%v targetCalls=%d", result, err, targetCalls)
	}
}

func TestCheckInHeadersDoNotContaminateIdentityOrCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("UID") != "" || r.Header.Get("X-Checkin-Secret") != "" {
			t.Errorf("check-in headers leaked to %s", r.URL.Path)
		}
		switch r.URL.Path {
		case CallerIdentityPath:
			_, _ = w.Write([]byte(`{"principal_id":"user"}`))
		case BuiltinModelsPath:
			_, _ = w.Write([]byte(`{"builtinModels":[{"model_id":"test-model","model_name":"Test model"}]}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewCheckInClient(server.Client(), http.Header{"UID": {"account"}, "X-Checkin-Secret": {"private"}})
	client.stsHost, client.snapHost = server.URL, server.URL
	if _, err := client.GetUserInfo(context.Background(), "AK", "SK", "ST"); err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := client.FetchBuiltinModels(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK"}, "zh-cn"); err != nil {
		t.Fatalf("catalog: %v", err)
	}
}

type checkInCancelTransport func(*http.Request) (*http.Response, error)

func (f checkInCancelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDailyCheckInCancellationStopsLaterSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var steps []string
	client := NewCheckInClient(&http.Client{Transport: checkInCancelTransport(func(request *http.Request) (*http.Response, error) {
		steps = append(steps, request.URL.Path)
		body := `{"code":0,"data":{"items":[{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}]}}`
		if request.URL.Path == WelfareClaimPath {
			body = `{"code":0,"data":{"userBenefitId":"benefit"}}`
			cancel()
		} else if request.URL.Path != WelfareDeliveryPath {
			t.Errorf("cancellation did not stop step %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}, nil)
	result, err := client.DailyCheckIn(ctx, Credentials{AccessKey: "AK", SecretKey: "SK"})
	if err != nil || result == nil || !result.Claimed || result.ConfirmError == "" || strings.Join(steps, ",") != WelfareDeliveryPath+","+WelfareClaimPath {
		t.Fatalf("claim should remain successful without sending confirm/fallback after cancellation: result=%+v err=%v steps=%v", result, err, steps)
	}
}
