package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestProbeCB19(t *testing.T) {
	const authID = "probe-cb"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	service := &Service{cfg: &config.Config{
		CodeBuddyCNKey: []config.CodeBuddyCNKey{{APIKey: "k", BaseURL: "https://copilot.tencent.com/v2", Models: []config.CodeBuddyCNModel{{Name: "hy3"}}}},
	}}
	auth := &coreauth.Auth{ID: authID, Provider: constant.CodeBuddyCN,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "base_url": "https://copilot.tencent.com/v2"}}
	service.registerModelsForAuth(context.Background(), auth)
	got := modelRegistry.GetModelsForClient(authID)
	want := registry.GetCodeBuddyCNModels()
	gotIDs := map[string]bool{}
	for _, m := range got {
		gotIDs[m.ID] = true
	}
	t.Logf("got=%d want=%d", len(got), len(want))
	for _, m := range want {
		if !gotIDs[m.ID] {
			t.Logf("MISSING from registration: %s (type=%s ctx=%d)", m.ID, m.Type, m.ContextLength)
		}
	}
	for _, m := range got {
		t.Logf("  got: %s", m.ID)
	}
}
