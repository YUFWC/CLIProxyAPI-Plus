package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// V8OAuthStartProviders lists every provider StartOAuthV8 dispatches, keyed by the slug the console sends in ?provider=. The console moved every login to /v8/management/oauth/auth-url, so a provider missing here has no working login even though its v0 /<provider>-auth-url handler exists.
func (h *Handler) V8OAuthStartProviders() map[string]gin.HandlerFunc {
	return map[string]gin.HandlerFunc{
		"claude":        h.RequestAnthropicToken,
		"codex":         h.RequestCodexToken,
		"antigravity":   h.RequestAntigravityToken,
		"kimi":          h.RequestKimiToken,
		"kimi-ai":       h.RequestKimiAIToken,
		"codebuddy-cn":  h.RequestCodeBuddyCNToken,
		"codebuddy-ai":  h.RequestCodeBuddyAIToken,
		"minimax":       h.RequestMinimaxToken,
		"minimax-cn":    h.RequestMinimaxCNToken,
		"qoder-cn":      h.RequestQoderCNToken,
		"qoder-ai":      h.RequestQoderAIToken,
		"xai":           h.RequestXAIToken,
		"trae":          h.RequestTraeToken,
		"cline":         h.RequestClineToken,
		"floatboat":     h.RequestFloatboatToken,
		"devin":         h.RequestDevinToken,
		"xiaohuanxiong": h.RequestXiaohuanxiongToken,
		"codearts":      h.RequestCodeArtsToken,
		"meta":          h.RequestMetaToken,
	}
}

// StartOAuthV8 dispatches login using the provider query parameter.
func (h *Handler) StartOAuthV8(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	if start, ok := h.V8OAuthStartProviders()[provider]; ok {
		start(c)
		return
	}
	if !h.ServePluginAuthURL(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
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
