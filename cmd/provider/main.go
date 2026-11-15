// Command provider runs the Crossplane provider for Rauthy.
package main

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alecthomas/kingpin/v2"
	changelogsv1alpha1 "github.com/crossplane/crossplane-runtime/v2/apis/changelogs/proto/v1alpha1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/customresourcesgate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis"
	rauthycontroller "github.com/fluencelabs/crossplane-provider-rauthy/internal/controller"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/version"
)

const (
	leaseDuration = 60 * time.Second
	renewDeadline = 50 * time.Second
)

// flags holds the command line of the provider.
type flags struct {
	debug                    bool
	leaderElection           bool
	syncInterval             time.Duration
	pollInterval             time.Duration
	pollStateMetricInterval  time.Duration
	maxReconcileRate         int
	enableManagementPolicies bool
	enableChangeLogs         bool
	changelogsSocketPath     string
}

func parseFlags() flags {
	var f flags
	app := kingpin.New(filepath.Base(os.Args[0]), "Crossplane provider for Rauthy.").DefaultEnvars()
	app.Flag("debug", "Run with debug logging.").Short('d').BoolVar(&f.debug)
	app.Flag("leader-election", "Use leader election for the controller manager.").
		Short('l').Default("false").Envar("LEADER_ELECTION").BoolVar(&f.leaderElection)
	app.Flag("sync", "How often all resources are double-checked for drift.").
		Short('s').Default("1h").DurationVar(&f.syncInterval)
	app.Flag("poll", "How often individual resources are checked for drift.").
		Default("1m").DurationVar(&f.pollInterval)
	app.Flag("poll-state-metric", "State metric recording interval.").
		Default("5s").DurationVar(&f.pollStateMetricInterval)
	app.Flag("max-reconcile-rate", "The global maximum rate per second at which resources may be checked for drift.").
		Default("10").IntVar(&f.maxReconcileRate)
	app.Flag("enable-management-policies", "Enable support for management policies.").
		Default("true").Envar("ENABLE_MANAGEMENT_POLICIES").BoolVar(&f.enableManagementPolicies)
	app.Flag("enable-changelogs", "Enable capturing change logs during reconciliation.").
		Default("false").Envar("ENABLE_CHANGE_LOGS").BoolVar(&f.enableChangeLogs)
	app.Flag("changelogs-socket-path", "Path of the change logs socket, if enabled.").
		Default("/var/run/changelogs/changelogs.sock").
		Envar("CHANGELOGS_SOCKET_PATH").StringVar(&f.changelogsSocketPath)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	return f
}

func main() {
	f := parseFlags()

	zl := zap.New(zap.UseDevMode(f.debug))
	log := logging.NewLogrLogger(zl.WithName("crossplane-provider-rauthy"))
	if f.debug {
		// controller-runtime is very verbose even at info level, so it only
		// gets a real logger in debug mode.
		ctrl.SetLogger(zl)
	} else {
		ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	}

	mgr := newManager(f)
	o := controllerOptions(f, log)

	kingpin.FatalIfError(customresourcesgate.Setup(mgr, o), "Cannot set up the CRD gate controller")
	kingpin.FatalIfError(rauthycontroller.SetupGated(mgr, o), "Cannot set up the Rauthy controllers")
	kingpin.FatalIfError(mgr.Start(ctrl.SetupSignalHandler()), "Cannot start controller manager")
}

func newManager(f flags) ctrl.Manager {
	cfg, err := ctrl.GetConfig()
	kingpin.FatalIfError(err, "Cannot get API server rest config")

	scheme := runtime.NewScheme()
	kingpin.FatalIfError(clientgoscheme.AddToScheme(scheme), "Cannot add client-go scheme")
	kingpin.FatalIfError(apis.AddToScheme(scheme), "Cannot add Rauthy APIs to scheme")
	kingpin.FatalIfError(apiextensionsv1.AddToScheme(scheme), "Cannot add CustomResourceDefinition to scheme")

	lease, renew := leaseDuration, renewDeadline
	mgr, err := ctrl.NewManager(ratelimiter.LimitRESTConfig(cfg, f.maxReconcileRate), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			SyncPeriod: &f.syncInterval,
			// Strip the large schemas from CRDs before they enter the cache;
			// the safe-start gate only needs their existence and conditions.
			ByObject: map[client.Object]cache.ByObject{
				&apiextensionsv1.CustomResourceDefinition{}: {
					Transform: customresourcesgate.TransformStripCRDSchema,
				},
			},
		},
		Client: clientOptions(),

		// Leases only, and longer than the controller-runtime defaults: short
		// leases are lost under load when renewals miss their deadline.
		LeaderElection:             f.leaderElection,
		LeaderElectionID:           "crossplane-leader-election-provider-rauthy",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaseDuration:              &lease,
		RenewDeadline:              &renew,
	})
	kingpin.FatalIfError(err, "Cannot create controller manager")
	return mgr
}

// clientOptions keeps Secrets out of the manager's cache. The provider reads
// one API key and writes connection Secrets; served from the cache, the first
// read would start an informer that lists and watches every Secret in the
// cluster.
func clientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
	}
}

func controllerOptions(f flags, log logging.Logger) controller.Options {
	metricRecorder := managed.NewMRMetricRecorder()
	stateMetrics := statemetrics.NewMRStateMetrics()
	metrics.Registry.MustRegister(metricRecorder)
	metrics.Registry.MustRegister(stateMetrics)

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: f.maxReconcileRate,
		PollInterval:            f.pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobal(f.maxReconcileRate),
		Features:                &feature.Flags{},
		Gate:                    new(gate.Gate[schema.GroupVersionKind]),
		MetricOptions: &controller.MetricOptions{
			PollStateMetricInterval: f.pollStateMetricInterval,
			MRMetrics:               metricRecorder,
			MRStateMetrics:          stateMetrics,
		},
	}

	if f.enableManagementPolicies {
		o.Features.Enable(feature.EnableBetaManagementPolicies)
		log.Info("Beta feature enabled", "flag", feature.EnableBetaManagementPolicies)
	}

	if f.enableChangeLogs {
		o.Features.Enable(feature.EnableAlphaChangeLogs)
		log.Info("Alpha feature enabled", "flag", feature.EnableAlphaChangeLogs)

		conn, err := grpc.NewClient("unix://"+f.changelogsSocketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		kingpin.FatalIfError(err, "Cannot create change logs client connection at %s", f.changelogsSocketPath)

		o.ChangeLogOptions = &controller.ChangeLogOptions{
			ChangeLogger: managed.NewGRPCChangeLogger(
				changelogsv1alpha1.NewChangeLogServiceClient(conn),
				managed.WithProviderVersion("crossplane-provider-rauthy:"+version.Version)),
		}
	}
	return o
}
