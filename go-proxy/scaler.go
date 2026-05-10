package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const fieldManagerName = "ollama-gateway"

// Scaler manages the lifecycle of the worker Deployment: it scales up on
// demand, polls until at least one replica is Ready, and scales back down on
// idle. Concurrent scale-up requests are coalesced so a thundering herd of
// requests during a cold start results in a single Patch + a single poll loop.
type Scaler struct {
	log          *slog.Logger
	client       kubernetes.Interface
	deployment   string
	namespace    string
	readyTimeout time.Duration
	pollInterval time.Duration

	// ready is set when the most recent observation showed ReadyReplicas > 0.
	// It is cleared on scale-down and on upstream proxy errors so the next
	// request re-verifies before forwarding.
	ready atomic.Bool

	mu    sync.Mutex
	state *scaleState // non-nil while a scale-up is in flight
}

// scaleState is shared between the leader goroutine doing the scale-up and
// any waiters blocked on the result. err is written by the leader before
// close(done), so receivers reading after <-done observe it safely.
type scaleState struct {
	done chan struct{}
	err  error
}

func NewScaler(
	log *slog.Logger,
	client kubernetes.Interface,
	deployment, namespace string,
	readyTimeout, pollInterval time.Duration,
) *Scaler {
	return &Scaler{
		log:          log.With("component", "scaler", "deployment", deployment, "namespace", namespace),
		client:       client,
		deployment:   deployment,
		namespace:    namespace,
		readyTimeout: readyTimeout,
		pollInterval: pollInterval,
	}
}

// EnsureReady scales the deployment up to one replica if needed and blocks
// until at least one replica is Ready or ctx is cancelled. The first caller
// in any cold-start window does the work; subsequent callers wait on the same
// result.
func (s *Scaler) EnsureReady(ctx context.Context) error {
	if s.ready.Load() {
		return nil
	}

	s.mu.Lock()
	if s.ready.Load() {
		s.mu.Unlock()
		return nil
	}
	state := s.state
	leader := state == nil
	if leader {
		state = &scaleState{done: make(chan struct{})}
		s.state = state
	}
	s.mu.Unlock()

	if leader {
		go s.runScaleUp(state)
	}

	select {
	case <-state.done:
		return state.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scaler) runScaleUp(state *scaleState) {
	defer func() {
		s.mu.Lock()
		s.state = nil
		s.mu.Unlock()
		close(state.done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.readyTimeout)
	defer cancel()

	s.log.Info("scaling up to 1 replica")
	start := time.Now()

	if err := s.setReplicas(ctx, 1); err != nil {
		s.log.Error("scale-up patch failed", "err", err)
		state.err = err
		return
	}

	if err := s.waitReady(ctx); err != nil {
		s.log.Error("waiting for ready replicas failed", "err", err, "elapsed", time.Since(start))
		state.err = err
		return
	}

	s.ready.Store(true)
	s.log.Info("worker ready", "elapsed", time.Since(start).Round(time.Millisecond))
}

func (s *Scaler) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		deploy, err := s.client.AppsV1().Deployments(s.namespace).Get(ctx, s.deployment, metav1.GetOptions{})
		switch {
		case err != nil:
			s.log.Warn("k8s deployment get failed; will retry", "err", err)
		case deploy.Status.ReadyReplicas > 0:
			return nil
		default:
			s.log.Debug("worker still booting",
				"ready_replicas", deploy.Status.ReadyReplicas,
				"available_replicas", deploy.Status.AvailableReplicas,
				"replicas", deploy.Status.Replicas,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for ready replicas: %w", s.readyTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// ScaleToZero patches the deployment to 0 replicas and clears the ready flag.
func (s *Scaler) ScaleToZero(ctx context.Context) error {
	if err := s.setReplicas(ctx, 0); err != nil {
		return fmt.Errorf("scale to zero: %w", err)
	}
	s.ready.Store(false)
	return nil
}

// MarkUnready forces the next request to re-verify worker readiness. Called
// when an upstream proxy error suggests the worker may have died or been
// scaled down out-of-band.
func (s *Scaler) MarkUnready() {
	s.ready.Store(false)
}

func (s *Scaler) setReplicas(ctx context.Context, count int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, count))
	_, err := s.client.AppsV1().Deployments(s.namespace).Patch(
		ctx,
		s.deployment,
		types.MergePatchType,
		patch,
		metav1.PatchOptions{FieldManager: fieldManagerName},
	)
	if err != nil {
		return fmt.Errorf("patching replicas to %d: %w", count, err)
	}
	return nil
}

// ActivityTracker records the timestamp of the most recent inbound request.
// It is the input to the idle monitor's scale-down decision.
type ActivityTracker struct {
	mu   sync.Mutex
	last time.Time
}

func (a *ActivityTracker) Touch() {
	a.mu.Lock()
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *ActivityTracker) Last() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func (a *ActivityTracker) Reset() {
	a.mu.Lock()
	a.last = time.Time{}
	a.mu.Unlock()
}

// RunIdleMonitor blocks until ctx is cancelled. Every checkEvery interval it
// inspects the activity tracker; if the time since the last request exceeds
// idleTimeout it scales the worker to zero. If the scale-down patch fails, it
// is retried on the next tick (the tracker is not reset until the patch
// succeeds).
func RunIdleMonitor(
	ctx context.Context,
	log *slog.Logger,
	tracker *ActivityTracker,
	scaler *Scaler,
	idleTimeout, checkEvery time.Duration,
) {
	log = log.With("component", "idle-monitor")
	log.Info("started", "idle_timeout", idleTimeout.String(), "check_every", checkEvery.String())

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("stopped")
			return
		case <-ticker.C:
			last := tracker.Last()
			if last.IsZero() {
				continue
			}
			idleFor := time.Since(last)
			if idleFor < idleTimeout {
				continue
			}

			log.Info("idle threshold exceeded; scaling to zero", "idle_for", idleFor.Round(time.Second).String())
			scaleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := scaler.ScaleToZero(scaleCtx)
			cancel()
			if err != nil {
				log.Error("scale-to-zero failed; will retry on next tick", "err", err)
				continue
			}
			tracker.Reset()
		}
	}
}
