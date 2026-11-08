package controller

import (
	"context"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
)

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// PodGateReconciler maintains the pod condition dimp.research.io/patched for
// pods that declare it as a readiness gate. It closes the spec-drift window:
// a fresh pod that still runs the image's code (older than the fleet's
// in-place-patched version) is kept out of Service endpoints until the
// HotPatch reconciler has re-patched it.
type PodGateReconciler struct {
	client.Client
}

func hasGate(p *corev1.Pod) bool {
	for _, g := range p.Spec.ReadinessGates {
		if string(g.ConditionType) == v1.ConditionPatched {
			return true
		}
	}
	return false
}

// Reconcile sets the gate condition.
func (r *PodGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hasGate(&pod) || pod.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	latest, err := LatestSucceeded(ctx, r.Client, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	status, reason, msg := corev1.ConditionTrue, "NoPatch", "no in-place patch applies; image code is current"
	if latest != nil {
		got := pod.Annotations[v1.AnnotationPatchVersion]
		switch {
		case got == "":
			status, reason, msg = corev1.ConditionFalse, "SpecDrift", fmt.Sprintf("pod runs image code; fleet runs %s in place (HotPatch %s)", latest.Spec.Version, latest.Name)
		default:
			// Any recorded in-place version is acceptable: later HotPatches
			// (rolled back or not) manage annotated pods themselves.
			status, reason, msg = corev1.ConditionTrue, "Patched", "running in-place version "+got
		}
	}
	for _, c := range pod.Status.Conditions {
		if string(c.Type) == v1.ConditionPatched && c.Status == status && c.Reason == reason {
			return ctrl.Result{}, nil
		}
	}
	patch := client.MergeFrom(pod.DeepCopy())
	cond := corev1.PodCondition{Type: corev1.PodConditionType(v1.ConditionPatched), Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now()}
	replaced := false
	for i, c := range pod.Status.Conditions {
		if string(c.Type) == v1.ConditionPatched {
			pod.Status.Conditions[i], replaced = cond, true
		}
	}
	if !replaced {
		pod.Status.Conditions = append(pod.Status.Conditions, cond)
	}
	return ctrl.Result{}, r.Status().Patch(ctx, &pod, patch)
}

// SetupWithManager registers the reconciler; HotPatch changes requeue all
// pods in the namespace.
func (r *PodGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("podgate").
		For(&corev1.Pod{}).
		Watches(&v1.HotPatch{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			var pods corev1.PodList
			if err := mgr.GetClient().List(ctx, &pods, client.InNamespace(o.GetNamespace())); err != nil {
				return nil
			}
			var out []reconcile.Request
			for _, p := range pods.Items {
				if hasGate(&p) {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
				}
			}
			return out
		})).
		Complete(r)
}
