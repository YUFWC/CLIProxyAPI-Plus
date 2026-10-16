package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/checkin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAutoCheckinConfigReplacesHeadersAndRejectsInvalidWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "server: {port: 8317}\nrouting: {retry: {request-retry: 2}}\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/config/*path", h.ConfigV8)
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/config/automation/checkin/providers/codebuddy-cn", strings.NewReader(body)))
		return recorder
	}
	first := request(`{"enabled":false,"headers":{"UID":"${meta.uid}","X-Removed":"old"}}`)
	if first.Code != http.StatusOK {
		t.Fatalf("initial save: %d %s", first.Code, first.Body.String())
	}
	second := request(`{"enabled":false,"headers":{"UID":"${meta.uid}"}}`)
	if second.Code != http.StatusOK {
		t.Fatalf("replacement save: %d %s", second.Code, second.Body.String())
	}
	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := saved.AutoCheckin.Providers["codebuddy-cn"]
	if provider.Headers["UID"] != "${meta.uid}" || len(provider.Headers) != 1 || saved.RequestRetry != 2 {
		t.Fatal("replacement lost template/deletion or unrelated config")
	}
	var reply struct {
		Fingerprint string `json:"config_fingerprint"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Fingerprint != checkin.Fingerprint(provider.Effective()) {
		t.Fatal("saved provider fingerprint does not identify effective config")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := request(`{"enabled":true,"window":{"start":"03:10","end":"00:10"}}`)
	if bad.Code < 400 {
		t.Fatalf("invalid schedule accepted: %s", bad.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("invalid schedule changed persisted config")
	}
}
