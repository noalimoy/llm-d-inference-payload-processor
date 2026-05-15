/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package modelselector

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework"
)

func TestProcessRequestWritesSelectedModelToBodyAndCycleState(t *testing.T) {
	plugin, err := NewModelSelectorPlugin([]string{"model-a", "model-b", "model-c"})
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	request := framework.NewInferenceRequest()
	request.Body["model"] = "auto"
	cycleState := framework.NewCycleState()

	err = plugin.ProcessRequest(context.Background(), cycleState, request)
	if err != nil {
		t.Fatalf("ProcessRequest failed: %v", err)
	}

	selectedModel, ok := request.Body["model"].(string)
	if !ok || selectedModel == "" {
		t.Fatal("expected model field in request body to be set")
	}
	if selectedModel == "auto" {
		t.Error("expected model field to be replaced with selected model, still 'auto'")
	}

	storedModel, err := framework.ReadCycleStateKey[string](cycleState, SelectedModelKey)
	if err != nil {
		t.Fatalf("expected selected model in CycleState: %v", err)
	}
	if storedModel != selectedModel {
		t.Errorf("CycleState model %q does not match body model %q", storedModel, selectedModel)
	}
}

func TestProcessRequestSelectsFromConfiguredCandidates(t *testing.T) {
	candidates := []string{"llama-70b", "llama-8b", "mistral-7b"}
	plugin, err := NewModelSelectorPlugin(candidates)
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	request := framework.NewInferenceRequest()
	request.Body["model"] = "auto"
	cycleState := framework.NewCycleState()

	err = plugin.ProcessRequest(context.Background(), cycleState, request)
	if err != nil {
		t.Fatalf("ProcessRequest failed: %v", err)
	}

	selectedModel := request.Body["model"].(string)
	found := false
	for _, c := range candidates {
		if c == selectedModel {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("selected model %q is not in candidate list %v", selectedModel, candidates)
	}
}

func TestNewModelSelectorPluginRejectsEmptyCandidates(t *testing.T) {
	_, err := NewModelSelectorPlugin([]string{})
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
}

func TestFactoryParsesCandidatesFromConfig(t *testing.T) {
	config := ModelSelectorPluginConfig{
		Candidates: []string{"model-a", "model-b"},
	}
	rawParams, _ := json.Marshal(config)

	plugin, err := ModelSelectorPluginFactory("test-selector", rawParams, nil)
	if err != nil {
		t.Fatalf("factory failed: %v", err)
	}

	if plugin.TypedName().Name != "test-selector" {
		t.Errorf("expected name 'test-selector', got %q", plugin.TypedName().Name)
	}
	if plugin.TypedName().Type != ModelSelectorPluginType {
		t.Errorf("expected type %q, got %q", ModelSelectorPluginType, plugin.TypedName().Type)
	}
}

func TestFactoryRejectsEmptyCandidates(t *testing.T) {
	config := ModelSelectorPluginConfig{
		Candidates: []string{},
	}
	rawParams, _ := json.Marshal(config)

	_, err := ModelSelectorPluginFactory("test-selector", rawParams, nil)
	if err == nil {
		t.Fatal("expected error for empty candidates in factory")
	}
}

func TestFactoryRejectsInvalidJSON(t *testing.T) {
	_, err := ModelSelectorPluginFactory("test-selector", json.RawMessage(`{invalid`), nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestTypedName(t *testing.T) {
	plugin, _ := NewModelSelectorPlugin([]string{"model-a"})
	if plugin.TypedName().Type != ModelSelectorPluginType {
		t.Errorf("expected type %q, got %q", ModelSelectorPluginType, plugin.TypedName().Type)
	}
}
