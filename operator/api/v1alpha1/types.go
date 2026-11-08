// Package v1alpha1 defines the HotPatch CRD (group dimp.research.io).
//
// A HotPatch asks the operator to replace the business logic of the pods
// selected by spec.selector IN PLACE (no pod replacement) with a signed WASM
// patch, validated in each pod by an in-process shadow + canary rollout,
// rolled out across the fleet in waves of maxUnavailablePods.
// +kubebuilder:object:generate=false
package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group/version.
var (
	GroupVersion  = schema.GroupVersion{Group: "dimp.research.io", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &HotPatch{}, &HotPatchList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Well-known names.
const (
	AnnotationPatchVersion = "dimp.research.io/patch-version" // version running in the pod
	AnnotationPatchDigest  = "dimp.research.io/patch-digest"  // sha256 of the running module
	AnnotationPatchName    = "dimp.research.io/hotpatch"      // HotPatch that installed it
	ConditionPatched       = "dimp.research.io/patched"       // pod readiness gate
)

// Phases.
const (
	PhasePending     = "Pending"
	PhaseProgressing = "Progressing"
	PhaseSucceeded   = "Succeeded"
	PhaseRollingBack = "RollingBack"
	PhaseRolledBack  = "RolledBack"
	PhaseFailed      = "Failed"
)

// Per-pod phases.
const (
	PodPending   = "Pending"
	PodWaiting   = "Waiting" // pod runs neither base nor target version yet
	PodPatching  = "Patching"
	PodPromoted  = "Promoted"
	PodFailed    = "Failed"
	PodRejected  = "Rejected"
	PodReverted  = "Reverted"
	PodAborted   = "Aborted"
	PodRepatched = "Repatched"
	// PodRollbackFailed: the fleet rollback could not revert/abort this pod
	// within the retry budget (operator gave up; manual action needed).
	PodRollbackFailed = "RollbackFailed"
)

// LocalRef names an object in the HotPatch namespace.
type LocalRef struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// Artifact locates the patch bundle.
type Artifact struct {
	// ConfigMapRef: binaryData keys manifest.json, module.wasm.gz
	// (gzip of the WASM module) and, unless signatureRef is set, manifest.sig.
	ConfigMapRef *LocalRef `json:"configMapRef,omitempty"`
	// URL of a JSON envelope (alternative to a ConfigMap; not used in the demo).
	URL string `json:"url,omitempty"`
}

// Step is one canary step.
type Step struct {
	Weight     int32            `json:"weight"` // percent of the pod's traffic
	Pause      *metav1.Duration `json:"pause,omitempty"`
	MinSamples int32            `json:"minSamples,omitempty"`
}

// SLO is the in-pod promotion criterion. Ratios are decimal strings.
type SLO struct {
	MaxErrorRateDelta string `json:"maxErrorRateDelta,omitempty"`
	MaxP99Factor      string `json:"maxP99Factor,omitempty"`
	P99SlackMs        int32  `json:"p99SlackMs,omitempty"`
	MaxMismatchRate   string `json:"maxMismatchRate,omitempty"`
	FailFastSamples   int32  `json:"failFastSamples,omitempty"`
	WarmupSamples     int32  `json:"warmupSamples,omitempty"`
}

// Strategy is the in-pod rollout schedule.
type Strategy struct {
	ShadowDuration *metav1.Duration `json:"shadowDuration,omitempty"`
	ShadowSamples  int32            `json:"shadowSamples,omitempty"`
	Steps          []Step           `json:"steps,omitempty"`
	SLO            SLO              `json:"slo,omitempty"`
}

// HotPatchSpec is the desired patch.
type HotPatchSpec struct {
	Selector metav1.LabelSelector `json:"selector"`
	// Version must equal the signed manifest's version.
	Version      string    `json:"version"`
	Artifact     Artifact  `json:"artifact"`
	SignatureRef *LocalRef `json:"signatureRef,omitempty"`
	Strategy     Strategy  `json:"strategy,omitempty"`
	// MaxUnavailablePods is the number of pods running a rollout at once
	// (pods never become unavailable; the name mirrors Deployment semantics).
	MaxUnavailablePods int32 `json:"maxUnavailablePods,omitempty"`
	AdminPort          int32 `json:"adminPort,omitempty"`
	// RepatchPolicy for pods that start later and run the image's code
	// (spec drift): Immediate (default) | Full (re-run the strategy) | Never.
	RepatchPolicy string `json:"repatchPolicy,omitempty"`
}

// PodMetrics summarises a pod's rollout analysis.
type PodMetrics struct {
	Stage            string `json:"stage,omitempty"`
	CandidateSamples int64  `json:"candidateSamples,omitempty"`
	CandidateErrRate string `json:"candidateErrorRate,omitempty"`
	CandidateP99Ms   string `json:"candidateP99Ms,omitempty"`
	StableP99Ms      string `json:"stableP99Ms,omitempty"`
	Mismatches       int64  `json:"mismatches,omitempty"`
	MismatchRate     string `json:"mismatchRate,omitempty"`
	DecisionMs       string `json:"decisionMs,omitempty"`
	VerifyMs         string `json:"verifyMs,omitempty"`
	CompileMs        string `json:"compileMs,omitempty"`
}

// PodStatus is one pod's state.
type PodStatus struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Phase string `json:"phase"`
	// FromVersion is the version the pod ran BEFORE this HotPatch touched it
	// (the fleet-rollback target), and FromDigest its digest when known.
	FromVersion string `json:"fromVersion,omitempty"`
	FromDigest  string `json:"fromDigest,omitempty"`
	// Attempts counts consecutive transient failures (push, abort,
	// rollback) for bounded retry with backoff.
	Attempts   int32        `json:"attempts,omitempty"`
	Version    string       `json:"version,omitempty"`
	Message    string       `json:"message,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	Metrics    *PodMetrics  `json:"metrics,omitempty"`
}

// HotPatchStatus is the observed state.
type HotPatchStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Digest             string             `json:"digest,omitempty"`
	BaseVersion        string             `json:"baseVersion,omitempty"`
	Message            string             `json:"message,omitempty"`
	Pods               []PodStatus        `json:"pods,omitempty"`
	Promoted           int32              `json:"promoted"`
	Total              int32              `json:"total"`
	StartedAt          *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt        *metav1.Time       `json:"completedAt,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// Attempts counts consecutive transient artifact-load failures.
	Attempts int32 `json:"attempts,omitempty"`
}

// HotPatch is the CRD.
type HotPatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HotPatchSpec   `json:"spec"`
	Status            HotPatchStatus `json:"status,omitempty"`
}

// HotPatchList is a list of HotPatch.
type HotPatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HotPatch `json:"items"`
}

// Deep copies: a JSON round trip keeps this prototype free of generated code
// (controller-gen would generate field-by-field copies; semantics are equal
// for these plain-data types).
func roundTrip(in, out any) {
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
}

// DeepCopyInto copies h into out.
func (h *HotPatch) DeepCopyInto(out *HotPatch) { *out = HotPatch{}; roundTrip(h, out) }

// DeepCopy returns a copy.
func (h *HotPatch) DeepCopy() *HotPatch {
	if h == nil {
		return nil
	}
	out := new(HotPatch)
	h.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (h *HotPatch) DeepCopyObject() runtime.Object { return h.DeepCopy() }

// DeepCopyInto copies l into out.
func (l *HotPatchList) DeepCopyInto(out *HotPatchList) { *out = HotPatchList{}; roundTrip(l, out) }

// DeepCopyObject implements runtime.Object.
func (l *HotPatchList) DeepCopyObject() runtime.Object {
	if l == nil {
		return nil
	}
	out := new(HotPatchList)
	l.DeepCopyInto(out)
	return out
}
