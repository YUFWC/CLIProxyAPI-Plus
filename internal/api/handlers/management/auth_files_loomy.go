package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	loomyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/loomy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// LoomySMSRequest asks the account service to deliver a verification code.
type LoomySMSRequest struct {
	// Phone is the 11-digit mainland phone number without a country prefix.
	Phone string `json:"phone"`
}

// LoomyLoginRequest completes the SMS login.
type LoomyLoginRequest struct {
	// Phone is the same number the code was sent to.
	Phone string `json:"phone"`
	// Code is the SMS verification code.
	Code string `json:"code"`
	// MsgID is the value from the send step.
	MsgID string `json:"msg_id"`
}

// RequestLoomySMSCode starts the Loomy login by sending an SMS code.
//
// Loomy has no browser authorization page, so the flow is code-based rather
// than URL-based: the panel collects a phone number here, then submits the code
// with the returned msg_id. No credential is persisted by this step.
func (h *Handler) RequestLoomySMSCode(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var req LoomySMSRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "phone is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	client := loomyauth.NewClient(h.cfg).WithHTTPClient(helps.NewProxyAwareHTTPClient(ctx, h.cfg, nil, 0))
	msgID, errSend := client.SendSMSCode(ctx, phone)
	if errSend != nil {
		status := http.StatusBadGateway
		if upstream, ok := errSend.(*loomyauth.Error); ok && upstream.StatusCode == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"status": "error", "error": errSend.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "flow": "sms", "msg_id": msgID})
}

// PostLoomyLogin completes the SMS login and persists the credential.
//
// A failed daily-allowance initialisation never fails the login: the session is
// already valid and the allowance is a separate, optional grant.
func (h *Handler) PostLoomyLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var req LoomyLoginRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}
	phone := strings.TrimSpace(req.Phone)
	code := strings.TrimSpace(req.Code)
	msgID := strings.TrimSpace(req.MsgID)
	if phone == "" || code == "" || msgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "phone, code and msg_id are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	client := loomyauth.NewClient(h.cfg).WithHTTPClient(helps.NewProxyAwareHTTPClient(ctx, h.cfg, nil, 0))
	token, errLogin := client.LoginWithSMSCode(ctx, phone, code, msgID)
	if errLogin != nil {
		status := http.StatusBadGateway
		if upstream, ok := errLogin.(*loomyauth.Error); ok && upstream.StatusCode != 0 {
			status = upstream.StatusCode
		}
		c.JSON(status, gin.H{"status": "error", "error": errLogin.Error()})
		return
	}
	if _, errClaim := client.ClaimDailyQuota(ctx, token.Session); errClaim != nil {
		log.Warnf("loomy: daily quota initialisation failed (login unaffected): %v", errClaim)
	}

	record := sdkauth.LoomySessionRecord(h.cfg, token, loomyauth.APIBase)
	// Use a stable file name derived from the account identity so a repeated
	// login for the same iFlytek account overwrites instead of duplicating.
	record.FileName = loomyAuthFileName(token)
	record.ID = record.FileName
	if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
		log.Errorf("loomy: save authentication failed: %v", errSave)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "could not save Loomy authentication"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "provider": constant.Loomy, "id": record.FileName, "label": record.Label})
}

// loomyAuthFileName derives a stable file name from the iFlytek account id.
func loomyAuthFileName(token *loomyauth.TokenData) string {
	seed := strings.TrimSpace(token.UserID)
	if seed == "" {
		seed = strings.TrimSpace(token.Session)
	}
	digest := sha256.Sum256([]byte(seed))
	return "loomy-" + hex.EncodeToString(digest[:16]) + ".json"
}

// GetLoomyQuota returns the read-only points view for one Loomy credential.
//
// Two pools are reported separately because Loomy computes them separately:
// permanent points and a daily allowance that does not refill after
// consumption. `daily_quota_known` distinguishes "not returned by a read" from
// "zero", and an unreadable response is an error rather than a zero balance.
func (h *Handler) GetLoomyQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		writeQuotaError(c, http.StatusBadRequest, "auth_index is required")
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), constant.Loomy) {
		writeQuotaError(c, http.StatusNotFound, "loomy credential not found")
		return
	}
	session := loomyQuotaSession(auth)
	if session == "" {
		writeQuotaError(c, http.StatusBadRequest, "loomy credential has no session")
		return
	}
	// Read from the same origin the executor uses for inference. The base URL is
	// part of the credential (a self-hosted deployment sets it), so the quota
	// view must not silently read a different host than the one serving turns.
	client := loomyauth.NewClientWithBaseURLs(h.cfg, auth.ProxyURL, loomyQuotaBaseURL(auth), "").
		WithHTTPClient(helps.NewProxyAwareHTTPClient(c.Request.Context(), h.cfg, auth, 0))
	detail, errQuota := client.FetchCreditDetail(c.Request.Context(), session)
	if errQuota != nil {
		status := http.StatusBadGateway
		if upstream, ok := errQuota.(*loomyauth.Error); ok &&
			(upstream.StatusCode == http.StatusUnauthorized || upstream.StatusCode == http.StatusForbidden) {
			status = upstream.StatusCode
		}
		writeQuotaError(c, status, errQuota.Error())
		return
	}
	out := gin.H{
		"permanent": detail.Permanent,
		"daily":     detail.Daily,
		"total":     detail.Total,
	}
	// The daily window fields only exist on responses that report them. Omitting
	// them keeps "unknown" distinct from a real zero.
	if detail.DailyQuotaKnown {
		out["daily_quota"] = detail.DailyQuota
	}
	if detail.DailyConsumed > 0 || detail.DailyQuotaKnown {
		out["daily_consumed"] = detail.DailyConsumed
	}
	if detail.DailyCycleDate != "" {
		out["daily_cycle_date"] = detail.DailyCycleDate
	}
	c.JSON(http.StatusOK, out)
}

// loomyQuotaBaseURL resolves the business origin recorded on the credential.
func loomyQuotaBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return loomyauth.APIBase
	}
	if auth.Attributes != nil {
		if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" {
			return base
		}
	}
	if auth.Metadata != nil {
		if base, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(base) != "" {
			return strings.TrimSpace(base)
		}
	}
	return loomyauth.APIBase
}

// loomyQuotaSession resolves the session that authorizes the points read.
func loomyQuotaSession(auth *coreauth.Auth) string {
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
