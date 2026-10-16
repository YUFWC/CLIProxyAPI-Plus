package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/checkin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAutoCheckinManagementRoutesRequireManagementAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name                      string
		authorized, enabled, home bool
		status                    int
	}{
		{"authorized", true, true, false, http.StatusOK},
		{"missing_key", false, true, false, http.StatusUnauthorized},
		{"management_disabled", true, false, false, http.StatusNotFound},
		{"home_execution_node", true, true, true, http.StatusNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("server: {port: 8317}\nmanagement: {secret-key: checkin-test-password}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Home.Enabled = scenario.home
			service := checkin.NewService(nil, checkin.NewCodeArtsAdapter(), checkin.NewCodeBuddyAdapter(), checkin.NewTraeAdapter())
			if err := service.Start(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Stop)
			handler := management.NewHandler(cfg, path, nil)
			handler.SetLocalPassword("checkin-test-password")
			handler.SetAutoCheckinService(service)
			server := &Server{cfg: cfg, engine: gin.New(), mgmt: handler}
			server.managementRoutesEnabled.Store(scenario.enabled)
			server.registerManagementRoutes()
			for _, route := range []string{"/v8/management/automation/checkin/providers", "/v8/management/automation/checkin/status"} {
				req := httptest.NewRequest(http.MethodGet, route, nil)
				req.RemoteAddr = "127.0.0.1:1234"
				if scenario.authorized {
					req.Header.Set("Authorization", "Bearer checkin-test-password")
				}
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, req)
				if response.Code != scenario.status {
					t.Fatalf("%s: got %d want %d: %s", route, response.Code, scenario.status, response.Body.String())
				}
			}
		})
	}
}
