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

func WithName(name string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.name = name
	}
}

func WithNamespace(ns string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.namespace = ns
	}
}

func WithID(id string) LeaseLockOption {
	return func(l *LeaseLock) {
		l.id = id
	}
}

func WithLeaseDuration(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.leaseDuration = d
	}
}

func WithRenewDeadline(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.renewDeadline = d
	}
}

func WithRetryPeriod(d time.Duration) LeaseLockOption {
	return func(l *LeaseLock) {
		l.retryPeriod = d
	}
}

func WithClient(client kubernetes.Interface) LeaseLockOption {
	return func(l *LeaseLock) {
		l.client = client
	}
}

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

func Run(ctx context.Context, fn func(context.Context, *LeaseLock) error, opts Options) error {
	options := make([]LeaseLockOption, 0)
	options = append(options, WithName(opts.Name))
	options = append(options, WithNamespace(opts.Namespace))
	options = append(options, WithID(opts.ID))
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

	return fn(ctx, l)
}
