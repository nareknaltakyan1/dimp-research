package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
	"github.com/nareknaltakyan1/dimp-research/operator/internal/podclient"
)

// fakePod emulates an s7-inplace admin endpoint.
type fakePod struct {
	mu                sync.Mutex
	stable, previous  string
	phase, candidate  string
	onAbort           func(p *fakePod) (int, any)
	onRollback        func(p *fakePod, to string) (int, any)
	onPatch           func(p *fakePod) (int, any)
	abortN, rollbackN int
	patchN            int
}

func (p *fakePod) doc() podclient.RolloutDoc {
	d := podclient.RolloutDoc{Stable: p.stable, RunningDigest: "dig-" + p.stable, Previous: p.previous}
	if p.previous != "" {
		d.PreviousDigest = "dig-" + p.previous
	}
	if p.phase != "" {
		d.Rollout = &podclient.Rollout{Phase: p.phase, Stable: p.stable, Candidate: p.candidate}
	}
	return d
}

func (p *fakePod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	switch r.URL.Path {
	case "/admin/rollout":
		reply(200, p.doc())
	case "/admin/rollout/abort":
		p.abortN++
		reply(p.onAbort(p))
	case "/admin/rollback":
		p.rollbackN++
		if p.onRollback == nil {
			reply(500, map[string]any{"error": "boom"})
			return
		}
		reply(p.onRollback(p, r.URL.Query().Get("to")))
	case "/admin/patch":
		p.patchN++
		reply(p.onPatch(p))
	default:
		http.NotFound(w, r)
	}
}

// retainedRollback is the correct pod behaviour: only the retained version.
func retainedRollback(p *fakePod, to string) (int, any) {
	if p.previous == "" || to != p.previous {
		return 409, map[string]any{"error": "not retained"}
	}
	p.stable, p.previous = p.previous, ""
	return 200, map[string]any{"stable": p.stable, "phase": "promoted"}
}

func setup(t *testing.T, fp *fakePod, phase string, ps v1.PodStatus) (*HotPatchReconciler, *v1.HotPatch, []corev1.Pod) {
	t.Helper()
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", UID: types.UID("uid-1"), Labels: map[string]string{"app": "s7"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	r := &HotPatchReconciler{
		Client: cl, Pods: podclient.New("tok"), Recorder: record.NewFakeRecorder(100),
		PodURL: func(*corev1.Pod, int32) string { return srv.URL },
	}
	ps.Name, ps.UID = pod.Name, string(pod.UID)
	hp := &v1.HotPatch{
		ObjectMeta: metav1.ObjectMeta{Name: "hp", Namespace: "ns"},
		Spec:       v1.HotPatchSpec{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "s7"}}, Version: "v2"},
		Status:     v1.HotPatchStatus{Phase: phase, Pods: []v1.PodStatus{ps}},
	}
	return r, hp, []corev1.Pod{pod}
}

// Regression (review Medium 3): the pod promoted the candidate before the
// operator's abort took effect. The old operator ignored the abort response
// and reported the pod Aborted / the fleet RolledBack while the pod kept
// running v2. Now the pod is re-observed, moved back to Promoted and
// reverted before the fleet is RolledBack.
func TestRollBackAbortTooLate(t *testing.T) {
	fp := &fakePod{stable: "v1", phase: "progressing", candidate: "v2", onRollback: retainedRollback}
	fp.onAbort = func(p *fakePod) (int, any) {
		p.stable, p.previous, p.phase = "v2", "v1", "promoted" // decision won the race
		return 409, map[string]any{"aborted": false, "sent": false, "phase": "promoted", "stable": "v2", "candidate": "v2"}
	}
	r, hp, pods := setup(t, fp, v1.PhaseRollingBack, v1.PodStatus{Phase: v1.PodPatching, FromVersion: "v1", FromDigest: "dig-v1"})
	ctx := context.Background()
	if _, err := r.rollBack(ctx, hp, pods); err != nil {
		t.Fatal(err)
	}
	if ps := hp.Status.Pods[0]; ps.Phase != v1.PodPromoted || hp.Status.Phase != v1.PhaseRollingBack {
		t.Fatalf("after late abort: pod %s fleet %s (%s)", ps.Phase, hp.Status.Phase, ps.Message)
	}
	if _, err := r.rollBack(ctx, hp, pods); err != nil {
		t.Fatal(err)
	}
	ps := hp.Status.Pods[0]
	if ps.Phase != v1.PodReverted || ps.Version != "v1" || fp.stable != "v1" || hp.Status.Phase != v1.PhaseRolledBack {
		t.Fatalf("want reverted to v1: pod %+v fleet %s pod-stable %s", ps, hp.Status.Phase, fp.stable)
	}
}

// An acknowledged abort is still verified against the pod's state.
func TestRollBackAbortVerified(t *testing.T) {
	fp := &fakePod{stable: "v1", phase: "progressing", candidate: "v2"}
	fp.onAbort = func(p *fakePod) (int, any) {
		p.phase = "aborted"
		return 200, map[string]any{"aborted": true, "sent": true, "phase": "aborted", "stable": "v1", "candidate": "v2"}
	}
	r, hp, pods := setup(t, fp, v1.PhaseRollingBack, v1.PodStatus{Phase: v1.PodPatching, FromVersion: "v1"})
	r.rollBack(context.Background(), hp, pods)
	if ps := hp.Status.Pods[0]; ps.Phase != v1.PodAborted || ps.Version != "v1" || hp.Status.Phase != v1.PhaseRolledBack {
		t.Fatalf("pod %+v fleet %s", ps, hp.Status.Phase)
	}
}

// Regression (Medium 4): a rollback that keeps failing must not loop
// forever; after MaxAttempts the HotPatch is Failed with RollbackIncomplete.
func TestRollBackBoundedRetries(t *testing.T) {
	fp := &fakePod{stable: "v2", previous: "v1"} // onRollback nil -> HTTP 500
	r, hp, pods := setup(t, fp, v1.PhaseRollingBack, v1.PodStatus{Phase: v1.PodPromoted, FromVersion: "v1"})
	calls := 0
	for hp.Status.Phase == v1.PhaseRollingBack && calls < 3*MaxAttempts {
		res, err := r.rollBack(context.Background(), hp, pods)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if hp.Status.Phase == v1.PhaseRollingBack && res.RequeueAfter < Backoff(hp.Status.Pods[0].Attempts) {
			t.Fatalf("no backoff: %v after %d attempts", res.RequeueAfter, hp.Status.Pods[0].Attempts)
		}
	}
	c := meta.FindStatusCondition(hp.Status.Conditions, "Ready")
	if hp.Status.Phase != v1.PhaseFailed || calls != MaxAttempts || hp.Status.Pods[0].Phase != v1.PodRollbackFailed || c == nil || c.Reason != "RollbackIncomplete" {
		t.Fatalf("phase %s after %d calls, pod %+v, cond %+v", hp.Status.Phase, calls, hp.Status.Pods[0], c)
	}
}

// Regression (Medium 4): when the operator finds a pod already running the
// target (e.g. after an operator restart), the rollback target must be the
// pod's retained previous version, not the target itself (which made the
// old operator retry /admin/rollback?to=v2 forever).
func TestAlreadyPatchedRecordsTruePreviousVersion(t *testing.T) {
	fp := &fakePod{stable: "v2", previous: "v1", phase: "promoted", candidate: "v2", onRollback: retainedRollback}
	r, hp, pods := setup(t, fp, v1.PhaseProgressing, v1.PodStatus{Phase: v1.PodPending})
	b := &bundle{manifest: manifest{Version: "v2", BaseVersion: "v1"}}
	if _, err := r.progress(context.Background(), hp, b, pods); err != nil {
		t.Fatal(err)
	}
	ps := hp.Status.Pods[0]
	if ps.Phase != v1.PodPromoted || ps.FromVersion != "v1" || ps.FromDigest != "dig-v1" {
		t.Fatalf("pod %+v", ps)
	}
	// And a fleet rollback now converges.
	hp.Status.Phase = v1.PhaseRollingBack
	r.rollBack(context.Background(), hp, pods)
	if hp.Status.Pods[0].Phase != v1.PodReverted || fp.stable != "v1" {
		t.Fatalf("rollback did not converge: %+v", hp.Status.Pods[0])
	}
}

// Regression (Medium 4): 409 RolloutInProgress (the pod is busy, e.g. with
// a re-patch) is retried with backoff instead of being a terminal
// rejection, and gives up after MaxAttempts.
func TestPushBusyIsRetriedThenBounded(t *testing.T) {
	fp := &fakePod{stable: "v1", phase: "progressing", candidate: "v9"}
	fp.onPatch = func(*fakePod) (int, any) {
		return 409, map[string]any{"reason": "RolloutInProgress", "error": "canary: rollout already in progress"}
	}
	r, hp, pods := setup(t, fp, v1.PhaseProgressing, v1.PodStatus{Phase: v1.PodPending})
	b := &bundle{envelope: []byte(`{}`), manifest: manifest{Version: "v2", BaseVersion: "v1"}}
	for i := 1; i < MaxAttempts; i++ {
		res, _ := r.progress(context.Background(), hp, b, pods)
		ps := hp.Status.Pods[0]
		if ps.Phase != v1.PodPending || ps.Attempts != int32(i) || hp.Status.Phase != v1.PhaseProgressing {
			t.Fatalf("attempt %d: pod %+v fleet %s", i, ps, hp.Status.Phase)
		}
		if res.RequeueAfter != Backoff(int32(i)) {
			t.Fatalf("attempt %d: requeue %v", i, res.RequeueAfter)
		}
	}
	r.progress(context.Background(), hp, b, pods)
	if ps := hp.Status.Pods[0]; ps.Phase != v1.PodRejected {
		t.Fatalf("after %d attempts: %+v", MaxAttempts, ps)
	}
	r.progress(context.Background(), hp, b, pods)
	if hp.Status.Phase != v1.PhaseFailed || fp.patchN != MaxAttempts {
		t.Fatalf("fleet %s pushes %d", hp.Status.Phase, fp.patchN)
	}
}

// Regression (Medium 4): a missing artifact ConfigMap is retried (bounded),
// not an immediate terminal failure.
func TestArtifactLoadRetried(t *testing.T) {
	fp := &fakePod{stable: "v1"}
	r, hp, _ := setup(t, fp, "", v1.PodStatus{Phase: v1.PodPending})
	hp.Status.Pods = nil
	hp.Spec.Artifact.ConfigMapRef = &v1.LocalRef{Name: "patch-v2"}
	for i := 1; i < MaxAttempts; i++ {
		res, _ := r.reconcile(context.Background(), hp)
		if hp.Status.Phase != "" || hp.Status.Attempts != int32(i) || res.RequeueAfter != Backoff(int32(i)) {
			t.Fatalf("attempt %d: phase %q attempts %d requeue %v", i, hp.Status.Phase, hp.Status.Attempts, res.RequeueAfter)
		}
	}
	r.reconcile(context.Background(), hp)
	if c := meta.FindStatusCondition(hp.Status.Conditions, "Ready"); hp.Status.Phase != v1.PhaseFailed || c == nil || c.Reason != "ArtifactUnavailable" {
		t.Fatalf("phase %s cond %+v", hp.Status.Phase, c)
	}
}
