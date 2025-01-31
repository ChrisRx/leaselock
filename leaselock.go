// Package leaselock provides a distributed mutex using the Kubernetes Lease
// API. It utilizes the Kubernetes leaderelection library, so has roughly the
// same guarantees as provided by Kubernetes for coordinated leader election.
package leaselock

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"go.chrisrx.dev/x/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

func init() {
	klog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

type LeaseLockOption func(*LeaseLock)

// WithName specifies the Kubernetes resource name for the lock.
func WithName(name string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.name = name
	}
}

// WithNamespace specifies the Kubernetes namespace where the lock will be
// created.
func WithNamespace(ns string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.namespace = ns
	}
}

// WithID overrides the default random ID with a user-provided ID.
func WithID(id string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.id = id
	}
}

// WithLeaseDuration specifies the lease duration for leader election. This
// value defaults to 60s and will determine how long non-leader candidates for
// a lock will wait before attempting to acquire leadership. Too small of a
// value may cause issues with leader election, so recommended to keep this
// above 10s.
func WithLeaseDuration(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.leaseDuration = d
	}
}

// WithRenewDeadline specifies the duration a leader will retry refreshing
// leadership before giving up. Defaults to 15s.
func WithRenewDeadline(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.renewDeadline = d
	}
}

// WithRetryPeriod specifies the duration between client actions.
func WithRetryPeriod(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.retryPeriod = d
	}
}

// WithClient allows passing in a Kubernetes client. By default, a client tries
// to use an in-cluster configuration, followed by attempting to read a
// kubeconfig from user configuration. This option is helpful in situations
// like unit testing, where a real cluster isn't being used.
func WithClient(client kubernetes.Interface) LeaseLockOption {
	return func(l *LeaseLock) {
		l.client = client
	}
}

// LeaseLock represents a mutex coordinated through Kubernetes leases. The
// leader election is provided by the Kubernetes leaderelection library, so
// guarantees on correctness will be based on what is provided by that package.
// In particular, it doesn't tolerate arbitrary clock skew rate so some of the
// tunables, like renew deadline, are really important to dealing with
// conditions in a cluster, such as high latency, that might cause leader
// election to fail (i.e. multiple leaders, no leaders, etc).
type LeaseLock struct {
	name          string
	namespace     string
	id            string
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	leader   atomic.Bool
	client   kubernetes.Interface
	elector  *leaderelection.LeaderElector
	isLeader chan struct{}
	stopped  chan struct{}
	mu       sync.Mutex
}

func New(ctx context.Context, opts ...LeaseLockOption) (_ *LeaseLock, err error) {
	l := &LeaseLock{
		name:          "lease-lock",
		namespace:     "default",
		id:            uuid.New().String(),
		leaseDuration: 60 * time.Second,
		renewDeadline: 15 * time.Second,
		retryPeriod:   5 * time.Second,
		isLeader:      make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}

	logger := log.FromContext(ctx).With(slog.String("id", l.id))

	l.ctx, l.cancel = context.WithCancel(ctx)
	l.ctx = log.Context(l.ctx, logger)

	if l.client == nil {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			cfg, err = clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube/config"))
			if err != nil {
				return nil, err
			}
		}
		l.client, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	l.elector, err = leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      l.name,
				Namespace: l.namespace,
			},
			Client: l.client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: l.id,
			},
		},
		ReleaseOnCancel: true,
		LeaseDuration:   l.leaseDuration,
		RenewDeadline:   l.renewDeadline,
		RetryPeriod:     l.retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				l.leader.Store(true)
				close(l.isLeader)
			},
			OnStoppedLeading: func() {
				defer close(l.stopped)
				if l.leader.Load() {
					logger.Debug("Performing cleanup operations...")
					return
				}
				logger.Debug("No cleanup needed as we never started leading.")
			},
			OnNewLeader: func(identity string) {
				if identity == l.id {
					return
				}
				logger.Debug("new leader elected", slog.String("newLeaderId", identity))
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return l, nil
}

func (l *LeaseLock) Lock() {
	l.mu.Lock()
	go l.elector.Run(l.ctx)
	<-l.isLeader
	l.isLeader = make(chan struct{})
}

func (l *LeaseLock) Unlock() {
	l.cancel()
	<-l.stopped
	l.stopped = make(chan struct{})
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.mu.Unlock()
}

func (l *LeaseLock) String() string {
	return l.id
}

type Options struct {
	Name          string
	Namespace     string
	ID            string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Run establishes a LeaseLock and only runs the provided callback when the
// participating client is elected leader. This should in most cases ensure
// that only the leader will be running the callback, however, the guarantees
// of this library are based on the guarantees of the Kubernetes leaderelection
// library, so if a stronger guarantee is required to ensure business-logic is
// executed only by one client at at a time, another distributed lock mechanism
// should be used.
func Run(ctx context.Context, fn func(context.Context, *LeaseLock) error, opts Options) error {
	options := make([]LeaseLockOption, 0)
	if opts.Name != "" {
		options = append(options, WithName(opts.Name))
	}
	if opts.Namespace != "" {
		options = append(options, WithNamespace(opts.Namespace))
	}
	if opts.ID != "" {
		options = append(options, WithID(opts.ID))
	}
	if opts.LeaseDuration != 0 {
		options = append(options, WithLeaseDuration(opts.LeaseDuration))
	}
	if opts.RenewDeadline != 0 {
		options = append(options, WithLeaseDuration(opts.RenewDeadline))
	}
	if opts.RetryPeriod != 0 {
		options = append(options, WithLeaseDuration(opts.RetryPeriod))
	}
	l, err := New(ctx, options...)
	if err != nil {
		return err
	}

	l.Lock()
	defer l.Unlock()

	// TODO(chrism): maybe log when this is finished so it is clear if the
	// callback function exited (vs. that it lost leadership for some reason so
	// all we get is the clean up message)
	return fn(ctx, l)
}
