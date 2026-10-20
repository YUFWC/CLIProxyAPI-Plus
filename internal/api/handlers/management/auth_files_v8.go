package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// StartOAuthV8 dispatches login using the provider query parameter.
func (h *Handler) StartOAuthV8(c *gin.Context) {
	switch strings.ToLower(strings.TrimSpace(c.Query("provider"))) {
	case "":
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
	case "claude":
		h.RequestAnthropicToken(c)
	case "codex":
		h.RequestCodexToken(c)
	case "antigravity":
		h.RequestAntigravityToken(c)
	case "kimi":
		h.RequestKimiToken(c)
	case "kimi-ai":
		h.RequestKimiAIToken(c)
	case "codebuddy-cn":
		h.RequestCodeBuddyCNToken(c)
	case "codebuddy-ai":
		h.RequestCodeBuddyAIToken(c)
	case "minimax":
		h.RequestMinimaxToken(c)
	case "minimax-cn":
		h.RequestMinimaxCNToken(c)
	case "qoder-cn":
		h.RequestQoderCNToken(c)
	case "qoder-ai":
		h.RequestQoderAIToken(c)
	case "xai":
		h.RequestXAIToken(c)
	case "trae":
		h.RequestTraeToken(c)
	case "cline":
		h.RequestClineToken(c)
	case "xiaohuanxiong":
		h.RequestXiaohuanxiongToken(c)
	case "codearts":
		h.RequestCodeArtsToken(c)
	case "devin":
		h.RequestDevinToken(c)
	case "meta":
		h.RequestMetaToken(c)
	case "floatboat":
		h.RequestFloatboatToken(c)
	case "lobsterai-server":
		h.RequestLobsteraiServerToken(c)
	case "loomy":
		// Loomy signs in with a phone SMS code rather than a URL, so the shared
		// auth-url contract cannot start it. Point the caller at the two steps.
		c.JSON(http.StatusBadRequest, gin.H{
			"status": "error",
			"error":  "loomy uses the SMS flow: POST /v8/management/loomy-sms-code then POST /v8/management/loomy-login",
			"flow":   "sms",
		})
	default:
		if !h.ServePluginAuthURL(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
		}
	}
}

// ImportOAuthV8 dispatches credential import using the provider query parameter.
func (h *Handler) ImportOAuthV8(c *gin.Context) {
	switch strings.ToLower(strings.TrimSpace(c.Query("provider"))) {
	case "":
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
	case "vertex":
		h.ImportVertexCredential(c)
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
	}
}
