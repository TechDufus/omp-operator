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
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

const (
	reasonMax = 300

	// restartedAtAnnotation is what kubectl rollout restart sets on the pod template.
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
)

type restartArgs struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// newWorkload returns an empty object of a restartable kind, or nil.
func newWorkload(kind v1alpha1.WorkloadKind) client.Object {
	switch kind {
	case v1alpha1.WorkloadDeployment:
		return &appsv1.Deployment{}
	case v1alpha1.WorkloadStatefulSet:
		return &appsv1.StatefulSet{}
	case v1alpha1.WorkloadDaemonSet:
		return &appsv1.DaemonSet{}
	}
	return nil
}

// restart patches restartedAt on a workload's pod template and waits for the rollout.
func (r *Runner) restart(ctx context.Context, c callCtx, spec *v1alpha1.KubernetesRolloutRestartAction, raw json.RawMessage) outcome {
	var a restartArgs
	if err := decodeArgs(raw, &a); err != nil {
		return refusal("", "%v", err)
	}
	if err := checkText("reason", a.Reason, reasonMax); err != nil {
		return refusal("", "%v", err)
	}
	kind := v1alpha1.WorkloadKind(a.Kind)
	if !slices.Contains(restartKinds(spec), kind) {
		return refusal("", "kind %q is not allowed; allowed kinds: %v", a.Kind, restartKinds(spec))
	}
	target := fmt.Sprintf("%s/%s/%s", kind, a.Namespace, a.Name)
	if a.Name == "" || len(a.Name) > 253 {
		return refusal(target, "name is required")
	}
	if !TargetNamespaceAllowed(r.Options, c.action.Namespace, spec.Namespaces, a.Namespace) {
		return refusal(target, "namespace %q is not allowed for this action", a.Namespace)
	}
	cooldown := durationOr(spec.Cooldown, defaultCooldown)
	if at, found, err := r.lastOKCall(ctx, c, target, cooldown); err != nil {
		return failure(target, "checking cooldown: %v", err)
	} else if found {
		return refusal(target, "%s was already restarted at %s; cooldown is %s", target, at.UTC().Format(time.RFC3339), cooldown)
	}

	obj := newWorkload(kind)
	key := client.ObjectKey{Namespace: a.Namespace, Name: a.Name}
	if err := r.APIReader.Get(ctx, key, obj); err != nil {
		return failure(target, "reading %s: %v", target, err)
	}
	restartedAt := r.now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, restartedAtAnnotation, restartedAt)
	if err := r.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return failure(target, "restarting %s: %v", target, err)
	}

	wait := durationOr(spec.WaitTimeout, defaultWaitTimeout)
	start := time.Now()
	wctx, cancel := r.waitContext(ctx, c, wait)
	defer cancel()
	counts := "no status observed"
	timedOut, err := r.pollUntil(wctx, func(ctx context.Context) (bool, error) {
		done, cnt, err := r.rolloutDone(ctx, kind, key)
		counts = cnt
		return done, err
	})
	if err != nil {
		return failure(target, "restart issued; could not read rollout status: %v", err)
	}
	if timedOut {
		return okOutcome(target, "restart issued; not ready after %s: %s", wait, counts)
	}
	return okOutcome(target, "restarted %s %s/%s; ready %s after %ds", kind, a.Namespace, a.Name, counts, int(time.Since(start).Seconds()))
}

// lastOKCall finds an ok call of the same tool on the same target within the window, among
// all sessions of the agent.
func (r *Runner) lastOKCall(ctx context.Context, c callCtx, target string, window time.Duration) (time.Time, bool, error) {
	var list v1alpha1.OmpSessionList
	if err := r.APIReader.List(ctx, &list,
		client.InNamespace(c.session.Namespace),
		client.MatchingLabels{v1alpha1.LabelAgent: c.agent.Name}); err != nil {
		return time.Time{}, false, err
	}
	now := r.now()
	var newest time.Time
	for i := range list.Items {
		for _, rec := range list.Items[i].Status.Actions {
			if rec.Action != c.tool || !rec.OK || rec.Target != target || now.Sub(rec.At.Time) >= window {
				continue
			}
			if rec.At.After(newest) {
				newest = rec.At.Time
			}
		}
	}
	return newest, !newest.IsZero(), nil
}

// rolloutDone reports whether a rollout is complete, with "ready/desired" counts, following
// kubectl rollout status.
func (r *Runner) rolloutDone(ctx context.Context, kind v1alpha1.WorkloadKind, key client.ObjectKey) (bool, string, error) {
	switch kind {
	case v1alpha1.WorkloadDeployment:
		var d appsv1.Deployment
		if err := r.APIReader.Get(ctx, key, &d); err != nil {
			return false, "", err
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		counts := fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, want)
		done := d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas >= want &&
			d.Status.Replicas <= d.Status.UpdatedReplicas &&
			d.Status.AvailableReplicas >= d.Status.UpdatedReplicas
		return done, counts, nil
	case v1alpha1.WorkloadStatefulSet:
		var s appsv1.StatefulSet
		if err := r.APIReader.Get(ctx, key, &s); err != nil {
			return false, "", err
		}
		want := int32(1)
		if s.Spec.Replicas != nil {
			want = *s.Spec.Replicas
		}
		counts := fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, want)
		if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			// Nothing rolls on its own; the template change is all that can be observed.
			return s.Status.ObservedGeneration >= s.Generation && s.Status.ReadyReplicas >= want, counts, nil
		}
		updated := want
		partitioned := false
		if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil && *ru.Partition > 0 {
			updated = max(want-*ru.Partition, 0)
			partitioned = true
		}
		done := s.Status.ObservedGeneration >= s.Generation &&
			s.Status.ReadyReplicas >= want &&
			s.Status.UpdatedReplicas >= updated &&
			(partitioned || s.Status.UpdateRevision == s.Status.CurrentRevision)
		return done, counts, nil
	case v1alpha1.WorkloadDaemonSet:
		var d appsv1.DaemonSet
		if err := r.APIReader.Get(ctx, key, &d); err != nil {
			return false, "", err
		}
		want := d.Status.DesiredNumberScheduled
		counts := fmt.Sprintf("%d/%d", d.Status.NumberReady, want)
		done := d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedNumberScheduled >= want &&
			d.Status.NumberAvailable >= want
		return done, counts, nil
	}
	return false, "", fmt.Errorf("unsupported kind %q", kind)
}
