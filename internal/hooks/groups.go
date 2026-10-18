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

package hooks

import (
	"context"
	"slices"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/episodes"
	"github.com/TechDufus/omp-operator/internal/metrics"
)

func stateFromAPI(g v1alpha1.TriggerGroup) episodes.GroupState {
	s := episodes.GroupState{
		ID:                  g.ID,
		Name:                g.Name,
		Severity:            g.Severity,
		EpisodeStart:        g.EpisodeStart.Time,
		LastNotified:        g.LastNotified.Time,
		TriagedFingerprints: slices.Clone(g.TriagedFingerprints),
		LastSession:         g.LastSession,
	}
	if g.ResolvedAt != nil {
		t := g.ResolvedAt.Time
		s.ResolvedAt = &t
	}
	if g.LastSessionAt != nil {
		t := g.LastSessionAt.Time
		s.LastSessionAt = &t
	}
	return s
}

func stateToAPI(s episodes.GroupState) v1alpha1.TriggerGroup {
	g := v1alpha1.TriggerGroup{
		ID:                  s.ID,
		Name:                s.Name,
		Severity:            s.Severity,
		TriagedFingerprints: slices.Clone(s.TriagedFingerprints),
		LastSession:         s.LastSession,
	}
	if !s.EpisodeStart.IsZero() {
		g.EpisodeStart = metav1.NewTime(s.EpisodeStart)
	}
	if !s.LastNotified.IsZero() {
		g.LastNotified = metav1.NewTime(s.LastNotified)
	}
	if s.ResolvedAt != nil {
		t := metav1.NewTime(*s.ResolvedAt)
		g.ResolvedAt = &t
	}
	if s.LastSessionAt != nil {
		t := metav1.NewTime(*s.LastSessionAt)
		g.LastSessionAt = &t
	}
	return g
}

func findGroup(groups []v1alpha1.TriggerGroup, id string) *v1alpha1.TriggerGroup {
	for i := range groups {
		if groups[i].ID == id {
			return &groups[i]
		}
	}
	return nil
}

// groupChanged reports whether state differs from the stored group (nil: none stored).
func groupChanged(stored *v1alpha1.TriggerGroup, state episodes.GroupState) bool {
	return stored == nil || !apiequality.Semantic.DeepEqual(*stored, stateToAPI(state))
}

// ActiveGroups counts the groups that are not resolved, for
// omp_operator_trigger_groups_active.
func ActiveGroups(groups []v1alpha1.TriggerGroup) int {
	n := 0
	for _, g := range groups {
		if g.ResolvedAt == nil {
			n++
		}
	}
	return n
}

// saveGroup merges state into the trigger's status.groups: it reads the
// trigger fresh, replaces or appends the group, prunes and patches with an
// optimistic lock, retrying on conflict.
func (h *Handler) saveGroup(ctx context.Context, key client.ObjectKey, state episodes.GroupState, now time.Time) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		t := &v1alpha1.OmpTrigger{}
		if err := h.APIReader.Get(ctx, key, t); err != nil {
			return err
		}
		orig := t.DeepCopy()
		states := make([]episodes.GroupState, 0, len(t.Status.Groups)+1)
		replaced := false
		for _, g := range t.Status.Groups {
			if g.ID == state.ID {
				states = append(states, state)
				replaced = true
				continue
			}
			states = append(states, stateFromAPI(g))
		}
		if !replaced {
			states = append(states, state)
		}
		pruned := episodes.Prune(states, now)
		groups := make([]v1alpha1.TriggerGroup, len(pruned))
		for i, s := range pruned {
			groups[i] = stateToAPI(s)
		}
		t.Status.Groups = groups
		if err := h.Client.Status().Patch(ctx, t, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		metrics.TriggerGroupsActive.WithLabelValues(key.Namespace, key.Name).Set(float64(ActiveGroups(groups)))
		return nil
	})
}
