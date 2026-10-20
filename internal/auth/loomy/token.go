package loomy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage persists a Loomy credential to the auth directory.
//
// The session is opaque and has no refresh counterpart, so the record carries no
// refresh token. `Expired` is derived locally from the login time because the
// upstream neither returns nor stores an absolute expiry.
type TokenStorage struct {
	// AccessToken is the 32-character hex iFlytek session.
	AccessToken string `json:"access_token"`
	// Expired is the RFC3339 locally derived expiry.
	Expired string `json:"expired,omitempty"`
	// UserID is the iFlytek user id.
	UserID string `json:"userid,omitempty"`
	// Phone is the bound phone number.
	Phone string `json:"phone,omitempty"`
	// Nickname is the optional display name.
	Nickname string `json:"nickname,omitempty"`
	// Type indicates the authentication provider type.
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks. It is not
	// exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// SetMetadata injects metadata into the storage before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the credential to an auth JSON file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = Provider
	if errMkdir := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errMkdir != nil {
		return fmt.Errorf("failed to create directory: %v", errMkdir)
	}
	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}
	file, errCreate := os.Create(authFilePath)
	if errCreate != nil {
		return fmt.Errorf("failed to create token file: %v", errCreate)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("loomy token storage: close token file error: %v", errClose)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(data); errEncode != nil {
		return fmt.Errorf("failed to write token to file: %v", errEncode)
	}
	return nil
}
