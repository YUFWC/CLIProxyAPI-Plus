package registry

import "testing"

// TestGetLoomyModelsLoadsEmbeddedCatalog verifies the dedicated embedded file is
// parsed. It is separate from models.json so a remote catalog refresh cannot
// erase this provider's fallback models.
func TestGetLoomyModelsLoadsEmbeddedCatalog(t *testing.T) {
	models := GetLoomyModels()
	if len(models) != 8 {
		t.Fatalf("fallback catalog size = %d, want 8 (the chat slice of the measured catalogue)", len(models))
	}
	ids := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			t.Fatalf("catalog contains an unusable entry: %+v", model)
		}
		if _, dup := ids[model.ID]; dup {
			t.Fatalf("duplicate model id %q", model.ID)
		}
		if model.Type != "loomy" {
			t.Fatalf("model %q type = %q", model.ID, model.Type)
		}
		ids[model.ID] = model
	}
	for _, want := range []string{
		"deepseek-v4-flash-0731", "MiniMax-M3", "Kimi-k2.6", "qwen-3.8-max",
		"GLM-5.3-Flash", "qwen3.8-flash", "spark-x", "mimo-v2.5",
	} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("catalog is missing %q", want)
		}
	}
	spark := ids["spark-x"]
	if spark.ContextLength != 1048576 {
		t.Fatalf("spark-x context window = %d, want the catalogue value 1048576", spark.ContextLength)
	}
	if spark.Thinking == nil || !spark.Thinking.ZeroAllowed {
		t.Fatalf("spark-x must declare a disable-able thinking menu: %+v", spark.Thinking)
	}
	if !containsLevel(spark.Thinking.Levels, "xhigh") {
		t.Fatalf("spark-x thinking levels = %v, want the measured set", spark.Thinking.Levels)
	}
}

// TestLoomyModelsResolveThroughStaticLookup verifies the channel dispatch and the
// generic lookup both reach the Loomy catalog.
func TestLoomyModelsResolveThroughStaticLookup(t *testing.T) {
	byChannel := LookupStaticModelInfoByChannel("mimo-v2.5", "loomy")
	if byChannel == nil || byChannel.DisplayName != "MiMo V2.5" {
		t.Fatalf("channel lookup = %+v", byChannel)
	}
	generic := LookupStaticModelInfo("mimo-v2.5")
	if generic == nil {
		t.Fatal("generic static lookup does not see the Loomy catalog")
	}
	if len(GetStaticModelDefinitionsByChannel("loomy")) != 8 {
		t.Fatal("channel dispatch does not return the Loomy catalog")
	}
}

func containsLevel(levels []string, want string) bool {
	for _, level := range levels {
		if level == want {
			return true
		}
	}
	return false
}
