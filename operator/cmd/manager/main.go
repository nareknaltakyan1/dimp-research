// Command manager runs the HotPatch operator.
package main

import (
	"flag"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/nareknaltakyan1/dimp-research/operator/api/v1alpha1"
	"github.com/nareknaltakyan1/dimp-research/operator/internal/controller"
	"github.com/nareknaltakyan1/dimp-research/operator/internal/podclient"
)

func main() {
	tokenFile := flag.String("admin-token-file", os.Getenv("ADMIN_TOKEN_FILE"), "bearer token for pod admin endpoints")
	metricsAddr := flag.String("metrics-addr", ":8082", "metrics address")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	lg := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	token := ""
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			lg.Error(err, "admin token")
			os.Exit(1)
		}
		token = strings.TrimSpace(string(b))
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: *metricsAddr}})
	if err != nil {
		lg.Error(err, "manager")
		os.Exit(1)
	}
	if err := (&controller.HotPatchReconciler{
		Client: mgr.GetClient(), Pods: podclient.New(token), Recorder: mgr.GetEventRecorderFor("hotpatch-operator"),
	}).SetupWithManager(mgr); err != nil {
		lg.Error(err, "hotpatch controller")
		os.Exit(1)
	}
	if err := (&controller.PodGateReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		lg.Error(err, "pod gate controller")
		os.Exit(1)
	}
	_ = corev1.SchemeGroupVersion
	lg.Info("starting HotPatch operator", "token", token != "")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		lg.Error(err, "run")
		os.Exit(1)
	}
}
