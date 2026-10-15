package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/paulgmiller/roachmotel/internal/operator"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		log.Fatal("POD_NAMESPACE is required")
	}
	template, err := operator.ValidateTemplate(os.Getenv("APPLICATION_URL_TEMPLATE"))
	if err != nil {
		log.Fatal(err)
	}
	if _, _, err := operator.NetworkService(template); err != nil {
		log.Fatal(err)
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("cannot configure Kubernetes client")
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("cannot create Kubernetes client")
	}
	excluded := map[string]bool{}
	for _, ns := range strings.Split(os.Getenv("EXCLUDED_NAMESPACES"), ",") {
		excluded[strings.TrimSpace(ns)] = true
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		// Re-read the mounted admin URL every pass, including after Secret changes.
		raw, err := os.ReadFile("/var/run/admin/connection-string")
		if err == nil {
			admin, parseErr := pgx.ParseConfig(strings.TrimSpace(string(raw)))
			if parseErr == nil {
				controller := operator.Controller{Client: client, DB: &operator.SQLDatabase{Config: admin}, Namespace: namespace, Template: template, Excluded: excluded}
				pass, cancel := context.WithTimeout(ctx, 30*time.Second)
				namespaces, listErr := client.CoreV1().Namespaces().List(pass, meta.ListOptions{})
				cancel()
				if listErr != nil {
					log.Print("namespace list failed; retrying")
				} else {
					for i := range namespaces.Items {
						attempt, done := context.WithTimeout(ctx, 15*time.Second)
						ns := &namespaces.Items[i]
						if err := controller.Reconcile(attempt, ns); err != nil {
							// SQL and Kubernetes errors can embed credentials; do not log raw errors.
							log.Printf("namespace %s reconciliation failed; retrying", ns.Name)
						}
						done()
						if ctx.Err() != nil {
							break
						}
					}
				}
			} else {
				log.Print("invalid admin connection configuration; retrying")
			}
		} else {
			log.Print("cannot read admin connection file; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
