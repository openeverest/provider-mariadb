// package main implements the provider-mariadb provider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/preflight"
	"github.com/openeverest/openeverest/v2/provider-runtime/reconciler"

	"github.com/openeverest/provider-mariadb/internal/provider"
)

// main is the entry point for the provider.
func main() {
	var serverPort int
	var metricsBindAddress string
	var admission bool
	var preflightSpec string
	var holdDataPlane bool
	var stopDeployment string

	flag.IntVar(&serverPort, "server-port", 8082, "The port for the provider HTTP server.")
	flag.StringVar(&metricsBindAddress, "metrics-bind-address", ":8081", "The address the metrics endpoint binds to. Use 0 to disable.")
	flag.BoolVar(&admission, "admission", true, "Serve the Instance admission webhook, with the serving certificate the chart mounts.")
	flag.StringVar(&preflightSpec, "preflight-spec", "", "Run the upgrade preflight against the target Provider spec at this path and exit. Used by the chart's pre-upgrade hook.")
	flag.BoolVar(&holdDataPlane, "hold-data-plane-updates", false, "With --preflight-spec, turn off automatic data-plane updates on every MariaDB once the preflight passes, before the operator is upgraded. Used by the chart's pre-upgrade hook.")
	flag.StringVar(&stopDeployment, "stop-provider-deployment", "", "With --hold-data-plane-updates, first scale this provider Deployment in the POD_NAMESPACE namespace to zero, so a provider release before maintenance gating cannot turn the updates back on. Used by the chart's pre-upgrade hook.")
	flag.Parse()

	l := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	p := provider.New()

	if preflightSpec != "" {
		// The controller-runtime logger is not set up on this path; stderr is
		// the pod log the hook keeps.
		if err := preflight.Run(ctx, p, preflight.Options{TargetSpecFile: preflightSpec}); err != nil {
			if !errors.Is(err, preflight.ErrUpgradeBlocked) {
				fmt.Fprintln(os.Stderr, "upgrade preflight could not run:", err)
			}
			os.Exit(1)
		}
		if holdDataPlane {
			if err := holdDataPlaneUpdates(ctx, p, stopDeployment); err != nil {
				fmt.Fprintln(os.Stderr, "upgrade preflight could not hold data-plane updates:", err)
				os.Exit(1)
			}
		}
		return
	}

	opts := []reconciler.ReconcilerOption{
		// Serve the health and readiness probes.
		reconciler.WithServer(reconciler.ServerConfig{Port: serverPort}),
	}

	if metricsBindAddress != "0" {
		opts = append(opts, reconciler.WithMetrics(metricsBindAddress))
	}
	if admission {
		opts = append(opts, reconciler.WithAdmission())
	}

	r, err := reconciler.New(ctx, p, opts...)
	if err != nil {
		l.Error(err, "unable to create reconciler")
		os.Exit(1)
	}
	p.SetClient(r.GetManager().GetClient())

	if err := r.Start(ctx); err != nil {
		l.Error(err, "unable to start reconciler")
		os.Exit(1)
	}
}

// holdDataPlaneUpdates runs provider.StopProvider, when given a Deployment, and
// provider.HoldDataPlaneUpdates with a direct client built from the in-cluster
// config.
func holdDataPlaneUpdates(ctx context.Context, p *provider.MariaDBProvider, stopDeployment string) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := p.Types()(scheme); err != nil {
		return err
	}
	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create the Kubernetes client: %w", err)
	}
	if stopDeployment != "" {
		if err := provider.StopProvider(ctx, cl, os.Getenv("POD_NAMESPACE"), stopDeployment, 2*time.Minute); err != nil {
			return err
		}
	}
	return provider.HoldDataPlaneUpdates(ctx, cl)
}
