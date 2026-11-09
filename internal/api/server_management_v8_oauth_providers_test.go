package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The console starts every login through /v8/management/oauth/auth-url?provider=<slug>; CodeBuddy, MiniMax, Qoder, Cline, Trae, Xiaohuanxiong and CodeArts kept their v0 /<slug>-auth-url routes but had no v8 case, so their logins answered provider_not_found.
func TestEveryV0LoginRouteHasAV8Provider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Dir(path)
	h := management.NewHandler(cfg, path, nil)
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.registerManagementRoutes()

	var v0 []string
	for _, route := range s.engine.Routes() {
		slug, ok := strings.CutPrefix(route.Path, "/v0/management/")
		if !ok || route.Method != http.MethodGet || !strings.HasSuffix(slug, "-auth-url") {
			continue
		}
		slug = strings.TrimSuffix(slug, "-auth-url")
		if slug == "anthropic" {
			slug = "claude"
		}
		v0 = append(v0, slug)
	}
	slices.Sort(v0)

	var v8 []string
	for slug := range h.V8OAuthStartProviders() {
		v8 = append(v8, slug)
	}
	slices.Sort(v8)

	want := []string{"antigravity", "claude", "cline", "codearts", "codebuddy-ai", "codebuddy-cn", "codex", "devin", "floatboat", "kimi", "kimi-ai", "meta", "minimax", "minimax-cn", "qoder-ai", "qoder-cn", "trae", "xai", "xiaohuanxiong"}
	if !slices.Equal(v0, want) {
		t.Fatalf("v0 login routes = %v, want %v", v0, want)
	}
	if !slices.Equal(v8, want) {
		t.Fatalf("v8 providers = %v, want %v", v8, want)
	}
}
