package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	lobsteraiserverauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/lobsteraiserver"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// LobsteraiServerQuota is the read-only credit/plan view of one LobsterAI
// credential, shaped for the management console quota page.
//
// The numbers come from the account backend's own reads
// (GET /api/user/quota and the client-activities slot/context). They are
// deliberately kept separate from:
//
//   - scheduler cooldown, which is local routing state, not an account balance;
//   - usage accounting, which counts tokens for a single request.
//
// The daily check-in activity is reported as observed state only. CLIProxyAPI
// never claims the reward on the account's behalf: check-in is a user action in
// the desktop client. An unknown value is reported as absent
// (CreditsKnown=false / ActivityAvailable=false) rather than as zero or
// unlimited.
type LobsteraiServerQuota struct {
	// SubscriptionStatus is the raw plan/status string (e.g. "free", "active").
	SubscriptionStatus string `json:"subscription_status,omitempty"`
	// Credits is the remaining credit balance when the backend reports one.
	Credits int64 `json:"credits"`
	// CreditsKnown reports whether Credits was present; false means unknown, not 0.
	CreditsKnown bool `json:"credits_known"`
	// PurchasedCredits is a separate credit bucket when the plan carries one.
	PurchasedCredits int64 `json:"purchased_credits,omitempty"`

	// ActivityAvailable reports whether a check-in/credit activity slot is open.
	ActivityAvailable bool `json:"activity_available"`
	// ActivityCode identifies the open activity (e.g. "daily_check_in").
	ActivityCode string `json:"activity_code,omitempty"`
	// ActivityTemplate is the activity's template when the backend reports one.
	ActivityTemplate string `json:"activity_template,omitempty"`
	// ActivityPlacement is the slot placement the read targeted.
	ActivityPlacement string `json:"activity_placement,omitempty"`
	// ActivityContext is the raw read-only activity context payload, if any.
	ActivityContext map[string]any `json:"activity_context,omitempty"`
}

// GetLobsteraiServerQuota returns the read-only credit/plan view for one
// LobsterAI credential, identified by auth_index. It never performs an activity
// action (check-in / claim) on the account's behalf.
func (h *Handler) GetLobsteraiServerQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		writeQuotaError(c, http.StatusBadRequest, "auth_index is required")
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), constant.LobsteraiServer) {
		writeQuotaError(c, http.StatusNotFound, "lobsterai-server credential not found")
		return
	}
	accessToken := lobsteraiServerQuotaAccessToken(auth)
	if accessToken == "" {
		writeQuotaError(c, http.StatusBadRequest, "lobsterai-server credential has no access token")
		return
	}

	backendURL := lobsteraiServerQuotaBaseURL(auth)
	client := lobsteraiserverauth.NewClientWithProxyURL(h.cfg, auth.ProxyURL, backendURL)

	quota, errQuota := client.FetchQuota(c.Request.Context(), accessToken)
	if errQuota != nil {
		writeQuotaError(c, http.StatusBadGateway, errQuota.Error())
		return
	}

	out := LobsteraiServerQuota{
		SubscriptionStatus: quota.SubscriptionStatus,
		Credits:            quota.Credits,
		CreditsKnown:       quota.CreditsKnown,
		PurchasedCredits:   quota.PurchasedCredits,
	}
	// The activity snapshot is best-effort: an unavailable activity must not
	// fail the whole quota read.
	if snapshot, errActivity := client.FetchActivitySnapshot(
		c.Request.Context(),
		accessToken,
		lobsteraiserverauth.ActivityPlacementDesktopSidebar,
		lobsteraiserverauth.ActivityContainerAPIVersionDailyCheckIn,
	); errActivity == nil && snapshot != nil {
		out.ActivityPlacement = snapshot.Placement
		out.ActivityAvailable = snapshot.SlotState == "available" && snapshot.ActivityCode != ""
		out.ActivityCode = snapshot.ActivityCode
		out.ActivityContext = snapshot.Context
		if snapshot.Context != nil {
			out.ActivityTemplate = lobsteraiServerQuotaString(snapshot.Context, "template", "activityTemplate")
		}
	}
	c.JSON(http.StatusOK, out)
}

// lobsteraiServerQuotaAccessToken resolves the account access token that
// authorizes the quota and activity reads.
func lobsteraiServerQuotaAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if token := strings.TrimSpace(auth.Attributes["access_token"]); token != "" {
			return token
		}
	}
	if auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// lobsteraiServerQuotaBaseURL resolves the account backend origin.
func lobsteraiServerQuotaBaseURL(auth *coreauth.Auth) string {
	if auth != nil {
		for _, source := range []map[string]string{auth.Attributes} {
			if source == nil {
				continue
			}
			if raw := strings.TrimRight(strings.TrimSpace(source["backend_url"]), "/"); raw != "" {
				return raw
			}
		}
	}
	return lobsteraiserverauth.DefaultBackendURL
}

// lobsteraiServerQuotaString reads the first non-empty string field.
func lobsteraiServerQuotaString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
