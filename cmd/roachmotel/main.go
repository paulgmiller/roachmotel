package main

import (
	"os"
	"strings"

	"github.com/paulgmiller/roachmotel/internal/operator"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	logger := zap.New()
	ctrl.SetLogger(logger)
	klog.SetLogger(logger)
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		logger.Error(nil, "POD_NAMESPACE is required")
		os.Exit(1)
	}
	template, err := operator.ValidateTemplate(os.Getenv("APPLICATION_URL_TEMPLATE"))
	if err != nil {
		logger.Error(err, "invalid application configuration")
		os.Exit(1)
	}
	if _, _, err := operator.NetworkService(template); err != nil {
		logger.Error(err, "invalid application configuration")
		os.Exit(1)
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "cannot configure Kubernetes client")
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error(err, "cannot create Kubernetes client")
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
		logger.Error(err, "cannot create controller manager")
		os.Exit(1)
	}
	controller := &operator.Controller{
		Client: client, Namespace: namespace, Template: template, Excluded: excluded,
	}
	if err := controller.SetupWithManager(mgr); err != nil {
		logger.Error(err, "cannot register namespace controller")
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "controller manager failed")
		os.Exit(1)
	}
}
