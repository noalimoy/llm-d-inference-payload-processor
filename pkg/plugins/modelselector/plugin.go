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
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-inference-payload-processor/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework"
	"github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/datalayer"
	fwkms "github.com/llm-d/llm-d-inference-payload-processor/pkg/framework/modelselector"
	ms "github.com/llm-d/llm-d-inference-payload-processor/pkg/modelselector"
)

const (
	ModelSelectorPluginType = "model-selector"

	// CycleState key where the selected model name is stored for downstream plugins.
	SelectedModelKey = "selected-model"
)

var _ framework.RequestProcessor = &ModelSelectorPlugin{}

// ModelSelectorPluginConfig holds the configuration parsed from plugin parameters.
type ModelSelectorPluginConfig struct {
	// Candidates is a static list of model names eligible for selection.
	// In this initial version, candidates are configured statically.
	// Future versions may source candidates from Datastore, CRD, or other dynamic sources.
	Candidates []string `json:"candidates"`
}

// ModelSelectorPluginFactory is the factory function for the ModelSelector RequestProcessor plugin.
func ModelSelectorPluginFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.Plugin, error) {
	var config ModelSelectorPluginConfig
	if rawParameters != nil {
		if err := json.Unmarshal(rawParameters, &config); err != nil {
			return nil, fmt.Errorf("failed to parse parameters for '%s' plugin: %w", ModelSelectorPluginType, err)
		}
	}

	if len(config.Candidates) == 0 {
		return nil, fmt.Errorf("'%s' plugin requires at least one candidate model", ModelSelectorPluginType)
	}

	plugin, err := NewModelSelectorPlugin(config.Candidates)
	if err != nil {
		return nil, err
	}

	if name != "" {
		plugin.typedName.Name = name
	}

	return plugin, nil
}

// NewModelSelectorPlugin creates a ModelSelector RequestProcessor plugin with the given candidate models.
func NewModelSelectorPlugin(candidateNames []string) (*ModelSelectorPlugin, error) {
	if len(candidateNames) == 0 {
		return nil, errors.New("at least one candidate model is required")
	}

	candidateModels := make([]datalayer.Model, len(candidateNames))
	for i, name := range candidateNames {
		candidateModels[i] = datalayer.NewModel(name)
	}

	// Build profile with picker only.
	// Filters and scorers can be added via WithFilters() / WithScorers()
	// once implementations are available.
	profile := ms.NewModelSelectorProfile().
		WithPicker(&defaultPicker{})

	selector := ms.NewModelSelector(profile)

	return &ModelSelectorPlugin{
		typedName:       framework.TypedName{Type: ModelSelectorPluginType, Name: ModelSelectorPluginType},
		selector:        selector,
		candidateModels: candidateModels,
	}, nil
}

// ModelSelectorPlugin is a RequestProcessor that runs the ModelSelector
// pipeline (Filter → Score → Pick) to select a model for the request.
type ModelSelectorPlugin struct {
	typedName       framework.TypedName
	selector        *ms.ModelSelector
	candidateModels []datalayer.Model
}

func (p *ModelSelectorPlugin) TypedName() framework.TypedName {
	return p.typedName
}

// ProcessRequest runs model selection and writes the selected model
// into the request body and CycleState.
func (p *ModelSelectorPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	logger := log.FromContext(ctx)

	result, err := p.selector.Select(ctx, request, cycleState, p.candidateModels)
	if err != nil {
		return fmt.Errorf("model selection failed: %w", err)
	}

	selectedName := result.TargetModel.GetName()
	logger.V(logutil.VERBOSE).Info("Model selected", "model", selectedName)

	cycleState.Write(SelectedModelKey, selectedName)
	request.SetBodyField("model", selectedName)

	return nil
}

// defaultPicker picks the first model from the scored list.
// This is a minimal picker for the initial version. Replace with
// MaxScorePicker or WeightedRandomPicker once available.
type defaultPicker struct{}

func (p *defaultPicker) TypedName() framework.TypedName {
	return framework.TypedName{Type: "default-picker", Name: "default-picker"}
}

func (p *defaultPicker) Pick(_ context.Context, _ *framework.CycleState, scoredModels []*fwkms.ScoredModel) *fwkms.ProfileRunResult {
	if len(scoredModels) == 0 {
		return nil
	}
	best := scoredModels[0]
	for _, sm := range scoredModels[1:] {
		if sm.Score > best.Score {
			best = sm
		}
	}
	return &fwkms.ProfileRunResult{TargetModel: best.Model}
}
