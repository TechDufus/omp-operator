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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// updateSessionStatus reads the session fresh, applies mutate and patches the status with an
// optimistic lock, retrying on conflict. mutate returns false when nothing changed.
func (s *Server) updateSessionStatus(ctx context.Context, key types.NamespacedName, mutate func(*v1alpha1.OmpSession) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sess v1alpha1.OmpSession
		if err := s.APIReader.Get(ctx, key, &sess); err != nil {
			return err
		}
		orig := sess.DeepCopy()
		if !mutate(&sess) {
			return nil
		}
		return s.Client.Status().Patch(ctx, &sess, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
}

// updateAgentStatus is updateSessionStatus for an OmpAgent.
func (s *Server) updateAgentStatus(ctx context.Context, key types.NamespacedName, mutate func(*v1alpha1.OmpAgent) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var agent v1alpha1.OmpAgent
		if err := s.APIReader.Get(ctx, key, &agent); err != nil {
			return err
		}
		orig := agent.DeepCopy()
		if !mutate(&agent) {
			return nil
		}
		return s.Client.Status().Patch(ctx, &agent, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
}

// markToolchainSynced records a confirmed toolchain: hash, syncedAt, stale=false, no error, ToolchainReady=True.
func (s *Server) markToolchainSynced(ctx context.Context, key types.NamespacedName, hash string) error {
	now := metav1.NewTime(s.now())
	return s.updateAgentStatus(ctx, key, func(a *v1alpha1.OmpAgent) bool {
		a.Status.Toolchain.Hash = hash
		a.Status.Toolchain.SyncedAt = &now
		a.Status.Toolchain.Stale = false
		a.Status.Toolchain.LastError = ""
		apimeta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionToolchainReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "toolchain is stored",
			ObservedGeneration: a.Generation,
		})
		return true
	})
}
