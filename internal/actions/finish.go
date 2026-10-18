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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	finishTitleMax    = 80
	finishSummaryMax  = 3000
	finishReportMax   = 20000
	finishCommandsMax = 10
	finishCommandMax  = 500

	// finishAlreadyRecorded is the answer to a second finish call.
	finishAlreadyRecorded = "finish already recorded"
)

var errFinishRecorded = errors.New(finishAlreadyRecorded)

type finishArgs struct {
	Verdict          string   `json:"verdict"`
	Title            string   `json:"title"`
	Summary          string   `json:"summary"`
	Report           string   `json:"report"`
	ProposedCommands []string `json:"proposed_commands"`
}

func (a *finishArgs) validate() error {
	if !slices.Contains(verdicts, a.Verdict) {
		return fmt.Errorf("verdict must be one of %v", verdicts)
	}
	if err := checkText("title", a.Title, finishTitleMax); err != nil {
		return err
	}
	if err := checkText("summary", a.Summary, finishSummaryMax); err != nil {
		return err
	}
	if err := checkText("report", a.Report, finishReportMax); err != nil {
		return err
	}
	if len(a.ProposedCommands) > finishCommandsMax {
		return fmt.Errorf("proposed_commands may have at most %d entries", finishCommandsMax)
	}
	for _, c := range a.ProposedCommands {
		if utf8.RuneCountInString(c) > finishCommandMax {
			return fmt.Errorf("each proposed command must be at most %d characters", finishCommandMax)
		}
	}
	return nil
}

var verdicts = []string{
	string(v1alpha1.VerdictOK), string(v1alpha1.VerdictFixed),
	string(v1alpha1.VerdictNeedsOwner), string(v1alpha1.VerdictInconclusive),
}

// finish stores the session's result: verdict, title, summary and proposed commands in the
// status, the report in the store. The first valid call wins; later ones are refused.
func (r *Runner) finish(ctx context.Context, agent *v1alpha1.OmpAgent, s *v1alpha1.OmpSession, raw json.RawMessage) (agentapi.ActionResult, error) {
	out := r.doFinish(ctx, s, raw)
	metrics.ActionCallsTotal.WithLabelValues(s.Namespace, agent.Name, FinishTool, out.result).Inc()
	if r.Store != nil {
		text := FinishTool + ": " + oneLine(out.text)
		if out.target != "" {
			text = FinishTool + " " + out.target + ": " + oneLine(out.text)
		}
		ev := store.Event{At: r.now().UTC(), Kind: "finish", Text: truncate(text, maxEventText)}
		if err := r.Store.AppendEvents(string(s.UID), []store.Event{ev}); err != nil {
			logf.FromContext(ctx).Error(err, "appending finish event", "session", client.ObjectKeyFromObject(s))
		}
	}
	return agentapi.ActionResult{Text: out.text, IsError: out.isErr}, nil
}

func (r *Runner) doFinish(ctx context.Context, s *v1alpha1.OmpSession, raw json.RawMessage) outcome {
	var a finishArgs
	if err := decodeArgs(raw, &a); err != nil {
		return refusal("", "%v", err)
	}
	if err := a.validate(); err != nil {
		return refusal("", "%v", err)
	}
	if r.Store == nil {
		return failure("", "report storage is not configured")
	}
	err := r.patchSessionStatus(ctx, client.ObjectKeyFromObject(s), func(cur *v1alpha1.OmpSession) error {
		if cur.Status.Verdict != "" {
			return errFinishRecorded
		}
		// The report is stored inside the check so a refused second call never overwrites it.
		if err := r.Store.WriteReport(string(cur.UID), a.Report); err != nil {
			return fmt.Errorf("storing report: %w", err)
		}
		cur.Status.Verdict = v1alpha1.Verdict(a.Verdict)
		cur.Status.Title = a.Title
		cur.Status.Summary = a.Summary
		cur.Status.ProposedCommands = a.ProposedCommands
		return nil
	})
	switch {
	case errors.Is(err, errFinishRecorded):
		return outcome{text: finishAlreadyRecorded, isErr: true, result: "refused"}
	case err != nil:
		return failure("", "could not record finish: %v", err)
	}
	return okOutcome(a.Verdict, "finish recorded")
}
