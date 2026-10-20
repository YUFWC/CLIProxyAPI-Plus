package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoLoomyLogin runs the iFlytek Loomy phone SMS login and saves the resulting
// session. Loomy has no browser-based flow: login always needs an interactive
// phone number and SMS code, so a caller without a prompt reports the missing
// prerequisite instead of starting a flow that cannot complete.
func DoLoomyLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	prompt := options.Prompt
	if prompt == nil {
		prompt = defaultProjectPrompt()
	}
	record, savedPath, err := newAuthManager().Login(context.Background(), "loomy", cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    prompt,
	})
	if err != nil {
		log.Errorf("Loomy authentication failed: %v", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Loomy authentication successful!")
}
