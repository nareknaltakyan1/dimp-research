// Package controller implements the HotPatch reconciler and the pod
// readiness-gate reconciler.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
	"github.com/nareknaltakyan1/dimp-research/operator/internal/podclient"
)

// HotPatchReconciler drives HotPatch objects.
type HotPatchReconciler struct {
	client.Client
	Pods     *podclient.Client
	Recorder record.EventRecorder
	// PodURL overrides how a pod's admin base URL is built (tests).
	PodURL func(pod *corev1.Pod, port int32) string
}

type manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	BaseVersion string `json:"base_version"`
	SHA256      string `json:"sha256"`
}

type bundle struct {
	envelope []byte
	manifest manifest
}

func cmKey(cm *corev1.ConfigMap, key string) ([]byte, bool) {
	if b, ok := cm.BinaryData[key]; ok {
		return b, true
	}
	if s, ok := cm.Data[key]; ok {
		return []byte(s), true
	}
	return nil, false
}

// loadBundle assembles the signed envelope from the artifact ConfigMap. The
// operator does NOT need to be trusted for integrity: the pod verifies the
// signature itself, so a compromised operator cannot inject code it cannot
// sign; the operator only parses the manifest to cross-check spec.version.
func (r *HotPatchReconciler) loadBundle(ctx context.Context, hp *v1.HotPatch) (*bundle, error) {
	ref := hp.Spec.Artifact.ConfigMapRef
	if ref == nil {
		return nil, fmt.Errorf("artifact.configMapRef is required (URL artifacts are not implemented)")
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: hp.Namespace, Name: ref.Name}, &cm); err != nil {
		return nil, fmt.Errorf("artifact configmap: %w", err)
	}
	man, ok1 := cmKey(&cm, "manifest.json")
	gz, ok2 := cmKey(&cm, "module.wasm.gz")
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("artifact configmap %s needs manifest.json and module.wasm.gz", ref.Name)
	}
	sigCM, sigKey := &cm, "manifest.sig"
	if sr := hp.Spec.SignatureRef; sr != nil {
		if sr.Key != "" {
			sigKey = sr.Key
		}
		if sr.Name != "" && sr.Name != ref.Name {
			sigCM = &corev1.ConfigMap{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: hp.Namespace, Name: sr.Name}, sigCM); err != nil {
				return nil, fmt.Errorf("signature configmap: %w", err)
			}
		}
	}
	sigFile, ok := cmKey(sigCM, sigKey)
	if !ok {
		return nil, fmt.Errorf("signature key %q not found", sigKey)
	}
	f := strings.Fields(string(sigFile))
	if len(f) != 2 {
		return nil, fmt.Errorf("malformed signature file")
	}
	sig, err := decodeB64(f[0])
	if err != nil {
		return nil, fmt.Errorf("malformed signature: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(man, &m); err != nil {
		return nil, fmt.Errorf("malformed manifest: %w", err)
	}
	env, _ := json.Marshal(map[string]any{"manifest": man, "signature": sig, "key_id": f[1], "module_gz": gz})
	return &bundle{envelope: env, manifest: m}, nil
}

func (r *HotPatchReconciler) podURL(p *corev1.Pod, port int32) string {
	if r.PodURL != nil {
		return r.PodURL(p, port)
	}
	return "http://" + net.JoinHostPort(p.Status.PodIP, strconv.Itoa(int(port)))
}

func podReady(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" && p.DeletionTimestamp == nil
}

func (r *HotPatchReconciler) targetPods(ctx context.Context, hp *v1.HotPatch) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(&hp.Spec.Selector)
	if err != nil {
		return nil, err
	}
	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(hp.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if podReady(&p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func now() *metav1.Time { t := metav1.Now(); return &t }

func (r *HotPatchReconciler) setCondition(hp *v1.HotPatch, typ string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&hp.Status.Conditions, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: hp.Generation})
}

func (r *HotPatchReconciler) annotate(ctx context.Context, pod *corev1.Pod, version, digest, hpName string) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[v1.AnnotationPatchVersion] = version
	pod.Annotations[v1.AnnotationPatchDigest] = digest
	pod.Annotations[v1.AnnotationPatchName] = hpName
	return r.Patch(ctx, pod, patch)
}

func fmtF(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

func metricsFrom(d *podclient.RolloutDoc) *v1.PodMetrics {
	m := &v1.PodMetrics{}
	if ro := d.Rollout; ro != nil {
		m.Stage = ro.Stage
		m.DecisionMs = fmtF(ro.DecisionMs)
		// The stage that decided (or the current one).
		if n := len(ro.Stages); n > 0 {
			s := ro.Stages[n-1]
			if s.Stage == "promote" && n > 1 {
				s = ro.Stages[n-2]
			}
			m.Stage = s.Stage
			m.CandidateSamples = s.Candidate.Count
			m.CandidateErrRate = fmtF(s.Candidate.ErrorRate)
			m.CandidateP99Ms = fmtF(s.Candidate.P99Ms)
			m.StableP99Ms = fmtF(s.Stable.P99Ms)
			m.Mismatches = s.Candidate.Mismatches
			m.MismatchRate = fmtF(s.Candidate.MismatchRate)
		}
	}
	if n := len(d.Patches); n > 0 {
		m.VerifyMs = fmtF(d.Patches[n-1].VerifyMs)
		m.CompileMs = fmtF(d.Patches[n-1].CompileMs)
	}
	return m
}

func (r *HotPatchReconciler) adminPort(hp *v1.HotPatch) int32 {
	if hp.Spec.AdminPort > 0 {
		return hp.Spec.AdminPort
	}
	return 9090
}

// Reconcile implements the control loop (see docs/solutions/S7-*.md).
func (r *HotPatchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	var hp v1.HotPatch
	if err := r.Get(ctx, req.NamespacedName, &hp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := hp.DeepCopy()
	res, err := r.reconcile(ctx, &hp)
	if err != nil {
		lg.Error(err, "reconcile")
	}
	hp.Status.ObservedGeneration = hp.Generation
	if !equalStatus(orig.Status, hp.Status) {
		if uerr := r.Status().Update(ctx, &hp); uerr != nil {
			if apierrors.IsConflict(uerr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, uerr
		}
	}
	return res, err
}

func equalStatus(a, b v1.HotPatchStatus) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func (r *HotPatchReconciler) reconcile(ctx context.Context, hp *v1.HotPatch) (ctrl.Result, error) {
	st := &hp.Status
	switch st.Phase {
	case v1.PhaseRolledBack, v1.PhaseFailed:
		return ctrl.Result{}, nil
	}
	b, err := r.loadBundle(ctx, hp)
	if err != nil {
		st.Phase, st.Message = v1.PhaseFailed, err.Error()
		r.setCondition(hp, "Ready", false, "ArtifactInvalid", err.Error())
		r.Recorder.Event(hp, corev1.EventTypeWarning, "ArtifactInvalid", err.Error())
		return ctrl.Result{}, nil
	}
	if b.manifest.Version != hp.Spec.Version {
		msg := fmt.Sprintf("manifest version %q != spec.version %q", b.manifest.Version, hp.Spec.Version)
		st.Phase, st.Message = v1.PhaseFailed, msg
		r.setCondition(hp, "Ready", false, "VersionMismatch", msg)
		return ctrl.Result{}, nil
	}
	st.Digest, st.BaseVersion = b.manifest.SHA256, b.manifest.BaseVersion
	pods, err := r.targetPods(ctx, hp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if st.Phase == "" {
		st.Phase, st.StartedAt = v1.PhaseProgressing, now()
		r.setCondition(hp, "Progressing", true, "Started", fmt.Sprintf("patching %d pods to %s", len(pods), hp.Spec.Version))
		r.Recorder.Eventf(hp, corev1.EventTypeNormal, "Started", "in-place patch to %s on %d pods", hp.Spec.Version, len(pods))
	}
	switch st.Phase {
	case v1.PhaseProgressing:
		return r.progress(ctx, hp, b, pods)
	case v1.PhaseRollingBack:
		return r.rollBack(ctx, hp, pods)
	case v1.PhaseSucceeded:
		return r.repatch(ctx, hp, b, pods)
	}
	return ctrl.Result{}, nil
}

func findPod(st *v1.HotPatchStatus, p *corev1.Pod) *v1.PodStatus {
	for i := range st.Pods {
		if st.Pods[i].UID == string(p.UID) {
			return &st.Pods[i]
		}
	}
	return nil
}

func (r *HotPatchReconciler) progress(ctx context.Context, hp *v1.HotPatch, b *bundle, pods []corev1.Pod) (ctrl.Result, error) {
	st := &hp.Status
	port := r.adminPort(hp)
	byUID := map[string]*corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		byUID[string(p.UID)] = p
		if findPod(st, p) == nil {
			st.Pods = append(st.Pods, v1.PodStatus{Name: p.Name, UID: string(p.UID), Phase: v1.PodPending})
		}
	}
	// Drop pods that disappeared before being patched.
	kept := st.Pods[:0]
	for _, ps := range st.Pods {
		if _, ok := byUID[ps.UID]; ok || ps.Phase == v1.PodPromoted || ps.Phase == v1.PodFailed || ps.Phase == v1.PodRejected {
			kept = append(kept, ps)
		}
	}
	st.Pods = kept

	// 1. Observe running rollouts and classify pending pods by version.
	for i := range st.Pods {
		ps := &st.Pods[i]
		pod := byUID[ps.UID]
		if pod == nil {
			continue
		}
		base := r.podURL(pod, port)
		switch ps.Phase {
		case v1.PodPatching:
			d, err := r.Pods.Rollout(ctx, base)
			if err != nil {
				ps.Message = err.Error()
				continue
			}
			ps.Metrics = metricsFrom(d)
			ro := d.Rollout
			if ro == nil || ro.Candidate != hp.Spec.Version {
				continue
			}
			switch ro.Phase {
			case "promoted":
				ps.Phase, ps.Version, ps.FinishedAt, ps.Message = v1.PodPromoted, ro.Stable, now(), ro.Reason
				if err := r.annotate(ctx, pod, hp.Spec.Version, d.RunningDigest, hp.Name); err != nil {
					return ctrl.Result{}, err
				}
				r.Recorder.Eventf(hp, corev1.EventTypeNormal, "PodPromoted", "%s now runs %s in place (decision %.0fms)", pod.Name, hp.Spec.Version, ro.DecisionMs)
			case "rolled_back", "aborted":
				ps.Phase, ps.Version, ps.FinishedAt, ps.Message = v1.PodFailed, ro.Stable, now(), ro.Reason
				r.Recorder.Eventf(hp, corev1.EventTypeWarning, "PodAnalysisFailed", "%s rolled back %s in-process: %s", pod.Name, hp.Spec.Version, ro.Reason)
			default:
				ps.Message = "stage " + ro.Stage
			}
		case v1.PodPending, v1.PodWaiting:
			d, err := r.Pods.Rollout(ctx, base)
			if err != nil {
				ps.Message = err.Error()
				ps.Phase = v1.PodWaiting
				continue
			}
			ps.FromVersion = d.Stable
			switch {
			case d.Stable == hp.Spec.Version:
				// Already patched (e.g. operator restarted mid-rollout).
				ps.Phase, ps.Version, ps.FinishedAt = v1.PodPromoted, d.Stable, now()
				if err := r.annotate(ctx, pod, hp.Spec.Version, d.RunningDigest, hp.Name); err != nil {
					return ctrl.Result{}, err
				}
			case b.manifest.BaseVersion == "" || d.Stable == b.manifest.BaseVersion:
				ps.Phase = v1.PodPending
			default:
				// Not on the base version yet (e.g. a fresh pod still being
				// re-patched by an earlier HotPatch): wait, don't fail.
				ps.Phase, ps.Message = v1.PodWaiting, fmt.Sprintf("pod runs %s, patch base is %s", d.Stable, b.manifest.BaseVersion)
			}
		}
	}

	// 2. Fleet decision.
	switch Decide(st.Pods) {
	case FleetRollback:
		st.Phase, st.Message = v1.PhaseRollingBack, "a pod's in-process analysis failed; halting and reverting the fleet"
		r.setCondition(hp, "Progressing", false, "AnalysisFailed", st.Message)
		r.Recorder.Event(hp, corev1.EventTypeWarning, "FleetRollback", st.Message)
		return r.rollBack(ctx, hp, pods)
	case FleetFailed:
		st.Phase, st.CompletedAt = v1.PhaseFailed, now()
		for _, ps := range st.Pods {
			if ps.Phase == v1.PodRejected {
				st.Message = fmt.Sprintf("pod %s rejected the bundle: %s", ps.Name, ps.Message)
				break
			}
		}
		r.setCondition(hp, "Ready", false, "Rejected", st.Message)
		r.setCondition(hp, "Progressing", false, "Rejected", st.Message)
		r.Recorder.Event(hp, corev1.EventTypeWarning, "Rejected", st.Message)
		r.count(hp)
		return ctrl.Result{}, nil
	case FleetSucceeded:
		st.Phase, st.CompletedAt, st.Message = v1.PhaseSucceeded, now(), fmt.Sprintf("all pods run %s in place", hp.Spec.Version)
		r.setCondition(hp, "Ready", true, "Succeeded", st.Message)
		r.setCondition(hp, "Progressing", false, "Succeeded", st.Message)
		r.Recorder.Event(hp, corev1.EventTypeNormal, "Succeeded", st.Message)
		r.count(hp)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 3. Start the next wave.
	maxU := int(hp.Spec.MaxUnavailablePods)
	strategy := PodStrategy(hp.Spec.Strategy)
	for _, i := range NextWave(st.Pods, maxU) {
		ps := &st.Pods[i]
		pod := byUID[ps.UID]
		if pod == nil {
			continue
		}
		code, rec, err := r.Pods.Push(ctx, r.podURL(pod, r.adminPort(hp)), b.envelope, strategy, "", false)
		ps.StartedAt = now()
		if err != nil {
			ps.Message = "push: " + err.Error() // transient: retry next reconcile
			continue
		}
		ps.Metrics = &v1.PodMetrics{VerifyMs: fmtF(rec.VerifyMs), CompileMs: fmtF(rec.CompileMs)}
		if code == 409 && (rec.Reason == "RolloutInProgress" || rec.Reason == "VersionPolicy") {
			// Idempotency: a previous push may have succeeded but its status
			// write was lost (update conflict). Ask the pod what it runs.
			if d, derr := r.Pods.Rollout(ctx, r.podURL(pod, r.adminPort(hp))); derr == nil {
				if d.Stable == hp.Spec.Version {
					ps.Phase, ps.Message = v1.PodPatching, "already promoted (duplicate push)"
					continue
				}
				if d.Rollout != nil && d.Rollout.Phase == "progressing" && d.Rollout.Candidate == hp.Spec.Version {
					ps.Phase, ps.Message = v1.PodPatching, "rollout already running (duplicate push)"
					continue
				}
			}
		}
		if code >= 300 {
			ps.Phase, ps.FinishedAt, ps.Message = v1.PodRejected, now(), fmt.Sprintf("HTTP %d %s: %s", code, rec.Reason, rec.Error)
			r.Recorder.Eventf(hp, corev1.EventTypeWarning, "PatchRejected", "%s rejected %s: %s %s", pod.Name, hp.Spec.Version, rec.Reason, rec.Error)
			continue
		}
		ps.Phase, ps.Message = v1.PodPatching, "accepted"
		r.Recorder.Eventf(hp, corev1.EventTypeNormal, "PodPatching", "%s verified %s (%.2fms) and started shadow/canary", pod.Name, hp.Spec.Version, rec.VerifyMs)
	}
	r.count(hp)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *HotPatchReconciler) count(hp *v1.HotPatch) {
	hp.Status.Total, hp.Status.Promoted = int32(len(hp.Status.Pods)), 0
	for _, p := range hp.Status.Pods {
		if p.Phase == v1.PodPromoted || p.Phase == v1.PodRepatched {
			hp.Status.Promoted++
		}
	}
}

// rollBack reverts every pod this HotPatch promoted and aborts running
// rollouts, then marks the HotPatch RolledBack.
func (r *HotPatchReconciler) rollBack(ctx context.Context, hp *v1.HotPatch, pods []corev1.Pod) (ctrl.Result, error) {
	st := &hp.Status
	byUID := map[string]*corev1.Pod{}
	for i := range pods {
		byUID[string(pods[i].UID)] = &pods[i]
	}
	pending := false
	for i := range st.Pods {
		ps := &st.Pods[i]
		pod := byUID[ps.UID]
		if pod == nil {
			continue
		}
		base := r.podURL(pod, r.adminPort(hp))
		switch ps.Phase {
		case v1.PodPromoted:
			if err := r.Pods.Rollback(ctx, base, ps.FromVersion); err != nil {
				ps.Message, pending = "rollback: "+err.Error(), true
				continue
			}
			ps.Phase, ps.Version, ps.Message = v1.PodReverted, ps.FromVersion, "fleet rollback"
			d, _ := r.Pods.Rollout(ctx, base)
			dig := ""
			if d != nil {
				dig = d.RunningDigest
			}
			if err := r.annotate(ctx, pod, ps.FromVersion, dig, hp.Name); err != nil {
				return ctrl.Result{}, err
			}
		case v1.PodPatching:
			if err := r.Pods.Abort(ctx, base, "fleet-rollback"); err != nil {
				ps.Message, pending = "abort: "+err.Error(), true
				continue
			}
			ps.Phase, ps.Message = v1.PodAborted, "aborted by fleet rollback"
		case v1.PodPending, v1.PodWaiting:
			ps.Phase, ps.Message = v1.PodAborted, "not started (fleet halted)"
		}
	}
	if pending {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	st.Phase, st.CompletedAt = v1.PhaseRolledBack, now()
	for _, ps := range st.Pods {
		if ps.Phase == v1.PodFailed {
			st.Message = fmt.Sprintf("rolled back: pod %s failed in-process analysis: %s", ps.Name, ps.Message)
			break
		}
	}
	r.setCondition(hp, "Ready", false, "RolledBack", st.Message)
	r.Recorder.Event(hp, corev1.EventTypeWarning, "RolledBack", st.Message)
	r.count(hp)
	return ctrl.Result{}, nil
}

// latestSucceeded returns the most recently completed Succeeded HotPatch in
// ns that selects pod (the fleet's desired code version for that pod).
func LatestSucceeded(ctx context.Context, c client.Client, pod *corev1.Pod) (*v1.HotPatch, error) {
	var list v1.HotPatchList
	if err := c.List(ctx, &list, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	var best *v1.HotPatch
	for i := range list.Items {
		h := &list.Items[i]
		if h.Status.Phase != v1.PhaseSucceeded || h.Status.CompletedAt == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&h.Spec.Selector)
		if err != nil || !sel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		if best == nil || h.Status.CompletedAt.After(best.Status.CompletedAt.Time) {
			best = h
		}
	}
	return best, nil
}

// repatch handles spec drift: a pod created after the patch (restart,
// scale-up, eviction) runs the IMAGE's code, so it is re-patched. Only the
// latest Succeeded HotPatch for the pod acts, and only on pods that carry no
// patch annotation (fresh pods).
func (r *HotPatchReconciler) repatch(ctx context.Context, hp *v1.HotPatch, b *bundle, pods []corev1.Pod) (ctrl.Result, error) {
	if hp.Spec.RepatchPolicy == "Never" {
		return ctrl.Result{}, nil
	}
	st := &hp.Status
	again := false
	for i := range pods {
		pod := &pods[i]
		if _, ok := pod.Annotations[v1.AnnotationPatchVersion]; ok {
			continue
		}
		latest, err := LatestSucceeded(ctx, r.Client, pod)
		if err != nil || latest == nil || latest.UID != hp.UID {
			continue
		}
		base := r.podURL(pod, r.adminPort(hp))
		d, err := r.Pods.Rollout(ctx, base)
		if err != nil {
			again = true
			continue
		}
		ps := findPod(st, pod)
		if ps == nil {
			st.Pods = append(st.Pods, v1.PodStatus{Name: pod.Name, UID: string(pod.UID), Phase: v1.PodPending, FromVersion: d.Stable})
			ps = &st.Pods[len(st.Pods)-1]
		}
		if d.Stable == hp.Spec.Version {
			ps.Phase, ps.Version, ps.FinishedAt, ps.Message = v1.PodRepatched, d.Stable, now(), "re-patched after pod (re)start (spec drift)"
			if err := r.annotate(ctx, pod, hp.Spec.Version, d.RunningDigest, hp.Name); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(hp, corev1.EventTypeNormal, "Repatched", "%s (new pod running image code %s) re-patched to %s", pod.Name, ps.FromVersion, hp.Spec.Version)
			continue
		}
		again = true
		if d.Rollout != nil && d.Rollout.Phase == "progressing" {
			continue
		}
		var strat *podclient.Strategy
		name := "immediate"
		if hp.Spec.RepatchPolicy == "Full" {
			strat = PodStrategy(hp.Spec.Strategy)
		}
		code, rec, err := r.Pods.Push(ctx, base, b.envelope, strat, name, true)
		if err != nil || code >= 300 {
			ps.Message = fmt.Sprintf("re-patch: %v %s %s", err, rec.Reason, rec.Error)
			continue
		}
		ps.Phase, ps.StartedAt, ps.Message = v1.PodPatching, now(), "re-patching (spec drift)"
	}
	r.count(hp)
	if again {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager registers the controller; pod events requeue every
// HotPatch in the pod's namespace (new pods may need re-patching).
func (r *HotPatchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.HotPatch{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			var list v1.HotPatchList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
				return nil
			}
			var out []reconcile.Request
			for _, h := range list.Items {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: h.Namespace, Name: h.Name}})
			}
			return out
		})).
		Complete(r)
}
