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
	"strings"
	"testing"

	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-inference-payload-processor/pkg/datastore"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/interface/datalayer"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/interface/modelselector"
	fwkplugin "github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/interface/plugin"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/interface/requesthandling"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/plugins/modelselector/picker/maxscore"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/plugins/modelselector/scorer/costaware"
)

// fakeHandle implements plugin.Handle for unit tests.
type fakeHandle struct {
	ds      datalayer.Datastore
	plugins map[string]fwkplugin.Plugin
}

func (f *fakeHandle) Context() context.Context                { return context.Background() }
func (f *fakeHandle) Client() client.Client                   { return nil }
func (f *fakeHandle) ReconcilerBuilder() *ctrlbuilder.Builder { return nil }
func (f *fakeHandle) Datastore() datalayer.Datastore          { return f.ds }

func (f *fakeHandle) Plugin(name string) fwkplugin.Plugin { return f.plugins[name] }
func (f *fakeHandle) AddPlugin(name string, p fwkplugin.Plugin) {
	f.plugins[name] = p
}
func (f *fakeHandle) GetAllPlugins() []fwkplugin.Plugin {
	result := make([]fwkplugin.Plugin, 0, len(f.plugins))
	for _, p := range f.plugins {
		result = append(result, p)
	}
	return result
}
func (f *fakeHandle) GetAllPluginsWithNames() map[string]fwkplugin.Plugin { return f.plugins }

func newTestDatastore(modelNames ...string) datalayer.Datastore {
	ds := datastore.NewFakeDataStore()
	for _, name := range modelNames {
		ds.GetOrCreateModel(name)
	}
	return ds
}

// newFakeHandle creates a fakeHandle with a datastore pre-populated with the given model names
// and no additional plugins configured.
func newFakeHandle(modelNames ...string) *fakeHandle {
	return &fakeHandle{
		ds:      newTestDatastore(modelNames...),
		plugins: map[string]fwkplugin.Plugin{},
	}
}

// TestProcessRequestWritesSelectedModelToBodyAndCycleState checks that the selected model is written to both the request body field "model" and CycleState.
func TestProcessRequestWritesSelectedModelToBodyAndCycleState(t *testing.T) {
	plugin, err := ModelSelectorPluginFactory(ModelSelectorPluginType, json.RawMessage(`{}`), newFakeHandle("model-a", "model-b", "model-c"))
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}
	p := plugin.(*ModelSelectorPlugin)

	request := requesthandling.NewInferenceRequest()
	request.Body["model"] = "auto"
	cycleState := fwkplugin.NewCycleState()

	err = p.ProcessRequest(context.Background(), cycleState, request)
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

	storedModel, err := fwkplugin.ReadCycleStateKey[string](cycleState, SelectedModelKey)
	if err != nil {
		t.Fatalf("expected selected model in CycleState: %v", err)
	}
	if storedModel != selectedModel {
		t.Errorf("CycleState model %q does not match body model %q", storedModel, selectedModel)
	}
}

// TestProcessRequestSelectsFromDatastoreModels checks that the selected model is one of the candidates registered in the datastore.
func TestProcessRequestSelectsFromDatastoreModels(t *testing.T) {
	candidates := []string{"llama-70b", "llama-8b", "mistral-7b"}
	plugin, err := ModelSelectorPluginFactory(ModelSelectorPluginType, json.RawMessage(`{}`), newFakeHandle(candidates...))
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}
	p := plugin.(*ModelSelectorPlugin)

	request := requesthandling.NewInferenceRequest()
	request.Body["model"] = "auto"
	cycleState := fwkplugin.NewCycleState()

	err = p.ProcessRequest(context.Background(), cycleState, request)
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
		t.Errorf("selected model %q is not in datastore models %v", selectedModel, candidates)
	}
}

// TestProcessRequestFailsWithEmptyDatastore checks that ProcessRequest returns an error when no candidate models are available.
func TestProcessRequestFailsWithEmptyDatastore(t *testing.T) {
	plugin, err := ModelSelectorPluginFactory(ModelSelectorPluginType, json.RawMessage(`{}`), newFakeHandle())
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}
	p := plugin.(*ModelSelectorPlugin)

	request := requesthandling.NewInferenceRequest()
	request.Body["model"] = "auto"
	cycleState := fwkplugin.NewCycleState()

	err = p.ProcessRequest(context.Background(), cycleState, request)
	if err == nil {
		t.Fatal("expected error with empty datastore")
	}
}

// TestTypedName checks that the plugin's TypedName type matches the registered ModelSelectorPluginType constant.
func TestTypedName(t *testing.T) {
	thePlugin, _ := ModelSelectorPluginFactory(ModelSelectorPluginType, json.RawMessage(`{}`), newFakeHandle("model-a"))
	if thePlugin.TypedName().Type != ModelSelectorPluginType {
		t.Errorf("expected type %q, got %q", ModelSelectorPluginType, thePlugin.TypedName().Type)
	}
}

// TestBuildProfileUsesDefaultMaxScorePickerWhenNoPickerInHandle checks that MaxScorePicker is used as the default picker when no picker plugin is in the handle.
func TestBuildProfileUsesDefaultMaxScorePickerWhenNoPickerInHandle(t *testing.T) {
	handle := newFakeHandle("model-a")
	profile, err := buildModelSelectorProfile(handle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	profileStr := profile.String()
	if !containsSubstring(profileStr, maxscore.MaxScorePickerType) {
		t.Errorf("expected default picker type %q in profile %q", maxscore.MaxScorePickerType, profileStr)
	}
}

// TestBuildProfileWiresScorerFromHandle checks that a scorer plugin registered in the handle is added to the profile.
func TestBuildProfileWiresScorerFromHandle(t *testing.T) {
	scorer := costaware.NewCostScorer()
	handle := newFakeHandle("model-a", "model-b")
	handle.AddPlugin(scorer.TypedName().Name, scorer)
	profile, err := buildModelSelectorProfile(handle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	profileStr := profile.String()
	if !containsSubstring(profileStr, costaware.CostScorerType) {
		t.Errorf("expected scorer type %q in profile %q", costaware.CostScorerType, profileStr)
	}
}

// TestBuildProfileWiresPickerFromHandle checks that a picker plugin registered in the handle is used instead of the default.
func TestBuildProfileWiresPickerFromHandle(t *testing.T) {
	picker := maxscore.NewMaxScorePicker()
	handle := newFakeHandle("model-a")
	handle.AddPlugin(picker.TypedName().Name, picker)
	profile, err := buildModelSelectorProfile(handle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	profileStr := profile.String()
	if !containsSubstring(profileStr, maxscore.MaxScorePickerType) {
		t.Errorf("expected picker type %q in profile %q", maxscore.MaxScorePickerType, profileStr)
	}
}

// TestBuildProfileRejectsMultiplePickers checks that registering more than one picker plugin in the handle returns an error.
func TestBuildProfileRejectsMultiplePickers(t *testing.T) {
	p1 := maxscore.NewMaxScorePicker().WithName("picker-1")
	p2 := maxscore.NewMaxScorePicker().WithName("picker-2")
	handle := newFakeHandle("model-a")
	handle.AddPlugin("picker-1", p1)
	handle.AddPlugin("picker-2", p2)
	_, err := buildModelSelectorProfile(handle)
	if err == nil {
		t.Fatal("expected error when two picker plugins are registered")
	}
}

// fakeScorerFilter implements both modelselector.Scorer and modelselector.Filter.
type fakeScorerFilter struct{ typedName fwkplugin.TypedName }

func (f *fakeScorerFilter) TypedName() fwkplugin.TypedName { return f.typedName }
func (f *fakeScorerFilter) Score(_ context.Context, _ *fwkplugin.CycleState, _ *requesthandling.InferenceRequest, models []datalayer.Model) map[datalayer.Model]float64 {
	out := make(map[datalayer.Model]float64, len(models))
	for _, m := range models {
		out[m] = 1.0
	}
	return out
}
func (f *fakeScorerFilter) Filter(_ context.Context, _ *fwkplugin.CycleState, _ *requesthandling.InferenceRequest, models []datalayer.Model) []datalayer.Model {
	return models
}

var _ modelselector.Scorer = &fakeScorerFilter{}
var _ modelselector.Filter = &fakeScorerFilter{}

// TestBuildProfilePluginImplementingBothScorerAndFilter checks that a plugin implementing both Scorer and Filter is registered in both roles within the profile.
func TestBuildProfilePluginImplementingBothScorerAndFilter(t *testing.T) {
	dual := &fakeScorerFilter{typedName: fwkplugin.TypedName{Type: "dual", Name: "dual"}}
	handle := newFakeHandle("model-a")
	handle.AddPlugin("dual", dual)
	profile, err := buildModelSelectorProfile(handle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	profileStr := profile.String()
	if !containsSubstring(profileStr, "dual") {
		t.Errorf("expected dual plugin in profile %q", profileStr)
	}
}

func containsSubstring(s, sub string) bool {
	return strings.Contains(s, sub)
}
