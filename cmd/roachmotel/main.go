package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/paulgmiller/roachmotel/internal/operator"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	klog.SetSlogLogger(slog.Default())
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		slog.Error("POD_NAMESPACE is required")
		os.Exit(1)
	}
	template, err := operator.ValidateTemplate(os.Getenv("APPLICATION_URL_TEMPLATE"))
	if err != nil {
		slog.Error("invalid application configuration", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	if _, _, err := operator.NetworkService(template); err != nil {
		slog.Error("invalid application configuration", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("cannot configure Kubernetes client", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		slog.Error("cannot create Kubernetes client", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	excluded := map[string]bool{}
	for _, ns := range strings.Split(os.Getenv("EXCLUDED_NAMESPACES"), ",") {
		excluded[strings.TrimSpace(ns)] = true
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		slog.Error("cannot create controller manager", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	controller := &operator.Controller{
		Client: client, Namespace: namespace, Template: template, Excluded: excluded,
	}
	if err := controller.SetupWithManager(mgr); err != nil {
		slog.Error("cannot register namespace controller", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("controller manager failed", "error", operator.ErrorSummary(err))
		os.Exit(1)
	}
}
