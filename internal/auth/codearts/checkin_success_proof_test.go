package codearts

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestCodeArtsCheckInNeedsExplicitClaimEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		fallback bool
		reply    string
		success  bool
	}{
		{name: "empty_primary_reply", reply: `{}`},
		{name: "null_primary_reply", reply: `null`},
		{name: "structured_benefit_identifier", reply: `{"data":{"userBenefitId":{}}}`},
		{name: "benefit_identifier_without_code", reply: `{"data":{"userBenefitId":"issued-benefit"}}`, success: true},
		{name: "empty_simple_benefit_reply", fallback: true, reply: `{}`},
		{name: "null_simple_benefit_reply", fallback: true, reply: `null`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case WelfareDeliveryPath:
					if scenario.fallback {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":"daily","type":"DAILY_CLAIM","claimable":true}]}}`)
				case WelfareClaimPath, BenefitClaimPath:
					fmt.Fprint(w, scenario.reply)
				case WelfareConfirmPath:
					fmt.Fprint(w, `{"code":0}`)
				default:
					t.Errorf("unexpected business path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			result, err := client.DailyCheckIn(context.Background(), Credentials{AccessKey: "AK", SecretKey: "SK"})
			if scenario.success {
				if err != nil || result == nil || !result.Claimed {
					t.Fatalf("issued benefit was not recognized: result=%+v err=%v", result, err)
				}
			} else if err == nil || result != nil {
				t.Fatalf("unproven HTTP 200 reply was treated as claimed: result=%+v err=%v", result, err)
			}
		})
	}
}
