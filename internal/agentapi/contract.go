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

package agentapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// TokenKey is the Secret key holding an API bearer token.
const TokenKey = "token"

// SessionTokenSecretName is the Secret holding a session's API token (owned by the session).
func SessionTokenSecretName(session string) string { return session + "-token" }

// ToolsyncTokenSecretName is the Secret holding an agent's toolsync API token (owned by the agent).
func ToolsyncTokenSecretName(agent string) string { return agent + "-toolsync-token" }

// ErrUnknownTool is returned by an ActionRunner for a tool the agent does not have; the API
// answers it with 404. The actions package aliases it as actions.ErrUnknownTool.
var ErrUnknownTool = errors.New("unknown tool")

// ActionRunner executes host tool calls and describes the available tools. It is satisfied
// by *actions.Runner (the actions package imports agentapi, so the server depends on this
// interface rather than the concrete type).
type ActionRunner interface {
	// ToolDefs returns one ToolDef per Ready OmpAction of the agent, then finish, and the resolved actions.
	ToolDefs(ctx context.Context, agent *v1alpha1.OmpAgent) ([]ToolDef, []v1alpha1.OmpAction, error)
	// Call runs one host tool call of a Running session. An unknown tool is ErrUnknownTool.
	Call(ctx context.Context, agent *v1alpha1.OmpAgent, s *v1alpha1.OmpSession, tool string, args json.RawMessage) (ActionResult, error)
}
