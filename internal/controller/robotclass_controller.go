/*
Copyright 2026 The Swarmada Authors.

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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// RobotClassReconciler maintains the read-only aggregate status of a RobotClass:
// how many Robots inherit from it (status.referencingRobots), and the class
// generation those admission-time merges draw from (status.observedGeneration).
//
// A RobotClass is a template with almost no runtime state; this controller adds no
// behavior beyond read/aggregate. It writes ONLY RobotClass.status — never Robot
// status — so it is orthogonal to the RA-1 telemetry-tick discipline.
type RobotClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=swarmada.io,resources=robotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robotclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch

// Reconcile counts the Robots in the class's namespace that reference it and records
// that count plus the observed (merge) generation on status. It writes only when the
// values change, so steady-state reconciles are free.
func (r *RobotClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var class fleetv1.RobotClass
	if err := r.Get(ctx, req.NamespacedName, &class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(class.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var count int32
	for i := range robots.Items {
		if robots.Items[i].Spec.RobotClass == class.Name {
			count++
		}
	}

	if class.Status.ReferencingRobots == count && class.Status.ObservedGeneration == class.Generation {
		return ctrl.Result{}, nil // status already current
	}
	class.Status.ReferencingRobots = count
	class.Status.ObservedGeneration = class.Generation
	if err := r.Status().Update(ctx, &class); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("RobotClass status updated",
		"referencingRobots", count, "observedGeneration", class.Generation)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller plus a watch on Robots so the
// referencing count stays live as robots are created, deleted, or re-classed.
func (r *RobotClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	robotToClass := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		robot, ok := obj.(*fleetv1.Robot)
		if !ok || robot.Spec.RobotClass == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{
			Namespace: robot.Namespace, Name: robot.Spec.RobotClass}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.RobotClass{}).
		Watches(&fleetv1.Robot{}, robotToClass).
		Complete(r)
}
