package registry

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// The Loomy catalog is embedded from a dedicated file rather than the shared
// models/models.json.
//
// The shared catalog is replaced wholesale by StartModelsUpdater when it fetches
// the upstream catalog, and that payload does not carry a loomy section. Keeping
// the models here means a remote refresh can never erase this provider's
// fallback catalog.
//
// This catalog is a fallback only: the Loomy business gateway owns the real
// per-account list (`GET /api/v1/models`), which the runtime probes and
// publishes per credential. The entries here mirror the 2026-09-26 measurement
// recorded by the reference implementation (8 of 11 catalogue rows are chat).
//
//go:embed models/loomy_models.json
var embeddedLoomyModelsJSON []byte

// loomyModelsFilePayload mirrors the on-disk shape.
type loomyModelsFilePayload struct {
	Loomy  []*ModelInfo `json:"loomy,omitempty"`
	Models []*ModelInfo `json:"models,omitempty"`
}

type loomyModelsStore struct {
	mu     sync.RWMutex
	models []*ModelInfo
}

var loomyCatalogStore = &loomyModelsStore{}

func init() {
	if _, errLoad := loadLoomyModelsFromBytes(embeddedLoomyModelsJSON, "embed"); errLoad != nil {
		log.Warnf("registry: failed to parse embedded loomy_models.json: %v", errLoad)
	}
}

// loadLoomyModelsFromBytes installs a catalog parsed from raw JSON.
func loadLoomyModelsFromBytes(data []byte, source string) ([]*ModelInfo, error) {
	var payload loomyModelsFilePayload
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	models := payload.Loomy
	if len(models) == 0 {
		models = payload.Models
	}
	normalized := normalizeLoomyModels(models)
	loomyCatalogStore.mu.Lock()
	loomyCatalogStore.models = normalized
	loomyCatalogStore.mu.Unlock()
	log.Debugf("registry: loaded %d loomy models from %s", len(normalized), source)
	return cloneModelInfos(normalized), nil
}

// normalizeLoomyModels fills defaults and drops unusable entries.
func normalizeLoomyModels(models []*ModelInfo) []*ModelInfo {
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		model.ID = id
		if strings.TrimSpace(model.Type) == "" {
			model.Type = "loomy"
		}
		if strings.TrimSpace(model.Object) == "" {
			model.Object = "model"
		}
		if strings.TrimSpace(model.OwnedBy) == "" {
			model.OwnedBy = "iflytek"
		}
		out = append(out, model)
	}
	return out
}

// GetLoomyModels returns the Loomy fallback catalog.
func GetLoomyModels() []*ModelInfo {
	loomyCatalogStore.mu.RLock()
	models := loomyCatalogStore.models
	loomyCatalogStore.mu.RUnlock()
	return cloneModelInfos(models)
}

// LookupLoomyModel resolves one model from the embedded fallback catalog.
func LookupLoomyModel(modelID string) *ModelInfo {
	clean := strings.TrimSpace(modelID)
	if clean == "" {
		return nil
	}
	loomyCatalogStore.mu.RLock()
	defer loomyCatalogStore.mu.RUnlock()
	for _, model := range loomyCatalogStore.models {
		if model != nil && model.ID == clean {
			return model
		}
	}
	return nil
}
