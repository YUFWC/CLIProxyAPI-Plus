package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/checkin"
)

// SetAutoCheckinService installs the service-owned scheduler; handlers never start jobs.
func (h *Handler) SetAutoCheckinService(service *checkin.Service) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.autoCheckinService = service
	h.mu.Unlock()
}

// GetAutoCheckinProviders describes protocols supported by this build.
func (h *Handler) GetAutoCheckinProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": checkin.Capabilities()})
}

// GetAutoCheckinStatus returns safe in-memory status without credential values.
func (h *Handler) GetAutoCheckinStatus(c *gin.Context) {
	provider := strings.TrimSpace(c.Query("provider"))
	if provider != "" {
		supported := false
		for _, capability := range checkin.Capabilities() {
			if capability.ID == provider {
				supported = true
				break
			}
		}
		if !supported {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_provider"})
			return
		}
	}
	h.mu.Lock()
	service := h.autoCheckinService
	h.mu.Unlock()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "automatic_checkin_unavailable"})
		return
	}
	c.JSON(http.StatusOK, service.Status(provider))
}
