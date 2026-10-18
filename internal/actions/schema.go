/*
Copyright 2026 TechDufus.

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

package actions

import (
	"encoding/json"
	"errors"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
)

type obj = map[string]any

// JSON Schema keys used more than once.
const (
	keyType        = "type"
	keyDescription = "description"
	keyEnum        = "enum"
	keyKind        = "kind"
	keyName        = "name"
	typeString     = "string"
	keyNamespace   = "namespace"
	keyReason      = "reason"
	keyMinLength   = "minLength"
	keyMaxLength   = "maxLength"
)

func objectSchema(required []string, props obj) obj {
	return obj{keyType: "object", "additionalProperties": false, "required": required, "properties": props}
}

func textProp(description string, limit int) obj {
	return obj{keyType: typeString, keyMinLength: 1, keyMaxLength: limit, keyDescription: description}
}

func nonEmptyString() obj { return obj{keyType: typeString, keyMinLength: 1} }

func finishDef() agentapi.ToolDef {
	schema := objectSchema([]string{"verdict", "title", "summary", "report"}, obj{
		"verdict": obj{
			keyType: typeString, keyEnum: verdicts,
			keyDescription: "ok: nothing needed doing; fixed: you resolved it; needs_owner: a human must act; inconclusive: you could not tell.",
		},
		"title":   textProp("One-line title of the outcome.", finishTitleMax),
		"summary": textProp("Short summary for the notification.", finishSummaryMax),
		"report":  textProp("The full report in Markdown: what you checked, found and did.", finishReportMax),
		"proposed_commands": obj{
			keyType: "array", "maxItems": finishCommandsMax,
			"items":        obj{keyType: typeString, keyMaxLength: finishCommandMax},
			keyDescription: "Commands an owner should run, if any.",
		},
	})
	return agentapi.ToolDef{
		Name:        FinishTool,
		Description: "Report the final result of this session. Call it exactly once, when you are done; the session ends after it.",
		Parameters:  mustJSON(schema),
	}
}

// parametersFor returns the JSON Schema of the arguments an action's tool takes.
func parametersFor(s *v1alpha1.OmpActionSpec) (json.RawMessage, error) {
	switch {
	case s.KubernetesRolloutRestart != nil:
		kinds := make([]string, 0, 3)
		for _, k := range restartKinds(s.KubernetesRolloutRestart) {
			kinds = append(kinds, string(k))
		}
		return mustJSON(objectSchema([]string{keyKind, keyNamespace, keyName, keyReason}, obj{
			keyKind:      obj{keyType: typeString, keyEnum: kinds},
			keyNamespace: nonEmptyString(),
			keyName:      nonEmptyString(),
			keyReason:    textProp("Why the restart is needed.", reasonMax),
		})), nil
	case s.FluxReconcile != nil:
		kinds := make([]string, 0, 5)
		for _, k := range fluxKinds(s.FluxReconcile) {
			kinds = append(kinds, string(k))
		}
		return mustJSON(objectSchema([]string{keyKind, keyNamespace, keyName, keyReason}, obj{
			keyKind:      obj{keyType: typeString, keyEnum: kinds},
			keyNamespace: nonEmptyString(),
			keyName:      nonEmptyString(),
			"with_source": obj{
				keyType: "boolean", keyDescription: "Also reconcile the object's source first.",
			},
			keyReason: textProp("Why the reconcile is needed.", reasonMax),
		})), nil
	case s.AlertmanagerSilence != nil:
		return mustJSON(objectSchema([]string{"matchers", "minutes", keyReason}, obj{
			"matchers": obj{
				keyType: "array", "minItems": 1, "maxItems": maxMatchers,
				keyDescription: "Equality matchers; exactly one must be on alertname.",
				"items": objectSchema([]string{keyName, "value"}, obj{
					keyName: nonEmptyString(),
					"value": obj{keyType: typeString, keyMinLength: 1, keyMaxLength: maxMatcherValue},
				}),
			},
			"minutes": obj{
				keyType: "integer", "minimum": minSilenceMinutes,
				keyDescription: "How long the silence lasts, in minutes.",
			},
			keyReason: textProp("Why the alert is silenced.", reasonMax),
		})), nil
	case s.HTTPRequest != nil:
		if len(s.HTTPRequest.Parameters.Raw) == 0 {
			return nil, errors.New("httpRequest.parameters is required")
		}
		return json.RawMessage(s.HTTPRequest.Parameters.Raw), nil
	}
	return nil, errors.New("no action type set")
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
