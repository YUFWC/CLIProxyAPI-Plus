package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoLobsteraiServerLogin starts the LobsterAI (NetEase/Youdao) login flow and
// saves the resulting credential. The flow serves a loopback callback at
// http://127.0.0.1:<port>/auth/callback and also accepts a pasted callback URL,
// so it works both locally and behind a remote browser.
func DoLobsteraiServerLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	record, savedPath, err := newAuthManager().Login(context.Background(), "lobsterai-server", cfg, &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		CallbackPort: options.CallbackPort,
		Metadata:     map[string]string{},
		Prompt:       options.Prompt,
	})
	if err != nil {
		log.Errorf("LobsterAI authentication failed: %v", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("LobsterAI authentication successful!")
}
