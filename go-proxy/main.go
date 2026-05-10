// Command ollama-gateway is the "scale-to-zero" sidecar for an Ollama worker
// Deployment. It receives requests, holds them in flight while it scales the
// worker up from zero, polls until the worker is Ready, then forwards the
// request. After IDLE_TIMEOUT of inactivity it scales the worker back down.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		// Fall back to a plain logger in case slog wasn't set up yet.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	log.Info("ollama gateway starting",
		"version", version,
		"commit", commit,
		"deployment", cfg.DeploymentName,
		"namespace", cfg.Namespace,
		"target", cfg.TargetURL,
		"idle_timeout", cfg.IdleTimeout.String(),
		"ready_timeout", cfg.ReadyTimeout.String(),
		"poll_interval", cfg.PollInterval.String(),
		"rate_rps", cfg.RateLimitRPS,
		"rate_burst", cfg.RateLimitBurst,
		"listen", cfg.ListenAddr,
	)

	targetURL, err := url.Parse(cfg.TargetURL)
	if err != nil {
		return fmt.Errorf("parsing OLLAMA_SERVICE_URL: %w", err)
	}
	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		return fmt.Errorf("OLLAMA_SERVICE_URL scheme must be http or https, got %q", targetURL.Scheme)
	}

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("loading in-cluster config (is this running inside a pod?): %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		return fmt.Errorf("creating k8s clientset: %w", err)
	}

	transport, err := buildUpstreamTransport(cfg)
	if err != nil {
		return fmt.Errorf("building upstream transport: %w", err)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scaler := NewScaler(log, k8sClient, cfg.DeploymentName, cfg.Namespace, cfg.ReadyTimeout, cfg.PollInterval)
	tracker := &ActivityTracker{}
	limiter := rate.NewLimiter(rate.Limit(cfg.RateLimitRPS), cfg.RateLimitBurst)

	handler := newGatewayHandler(log, targetURL, transport, scaler, tracker, limiter)

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: handler,
		// Slowloris protection on headers, but no write timeout: long Ollama
		// generations stream over many minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		RunIdleMonitor(rootCtx, log, tracker, scaler, cfg.IdleTimeout, cfg.IdleCheckInterval)
	}()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
	wg.Wait()
	log.Info("shutdown complete")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// buildUpstreamTransport returns an http.Transport configured for the worker.
// Optional CA / mTLS material is loaded when the corresponding env vars are
// set. ResponseHeaderTimeout is left at 0 to support streaming responses
// (e.g., Ollama's `/api/generate` line-delimited JSON stream).
func buildUpstreamTransport(cfg *Config) (*http.Transport, error) {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	needsTLS := cfg.UpstreamCACert != "" ||
		cfg.UpstreamClientCert != "" ||
		cfg.UpstreamServerName != "" ||
		cfg.UpstreamInsecureSkipVerify
	if !needsTLS {
		return t, nil
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.UpstreamInsecureSkipVerify, //nolint:gosec // explicit opt-in via env
		ServerName:         cfg.UpstreamServerName,
		MinVersion:         tls.VersionTLS12,
	}

	if cfg.UpstreamCACert != "" {
		caBytes, err := os.ReadFile(cfg.UpstreamCACert)
		if err != nil {
			return nil, fmt.Errorf("reading UPSTREAM_CA_CERT: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("UPSTREAM_CA_CERT contains no valid PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.UpstreamClientCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.UpstreamClientCert, cfg.UpstreamClientKey)
		if err != nil {
			return nil, fmt.Errorf("loading upstream client keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	t.TLSClientConfig = tlsCfg
	return t, nil
}

// ----- HTTP handler & middleware -----

func newGatewayHandler(
	log *slog.Logger,
	target *url.URL,
	transport http.RoundTripper,
	scaler *Scaler,
	tracker *ActivityTracker,
	limiter *rate.Limiter,
) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	// httputil's default Director rewrites req.URL but leaves req.Host alone,
	// so the upstream sees the *original* client's Host header. With an HTTPS
	// upstream that enforces strict SNI/Host matching (e.g. Caddy by default
	// for sites configured with client_auth, and several reverse proxies in
	// general) that mismatch produces a 403 before the request ever reaches
	// the application. Force the Host header to the upstream's host so SNI
	// and Host always agree.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
	}
	proxy.Transport = transport
	proxy.FlushInterval = -1 // immediate flush for streaming responses
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		rid := requestIDFromCtx(r.Context())
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Info("client disconnected during proxy", "request_id", rid, "err", err)
			return
		}
		log.Error("upstream proxy error",
			"request_id", rid,
			"method", r.Method,
			"path", r.URL.Path,
			"err", err,
		)
		// Worker may have died; force the next request to re-verify.
		scaler.MarkUnready()
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", healthHandler)
	mux.Handle("/", proxyHandler(log, proxy, scaler, tracker, limiter))

	return chainMiddleware(mux, requestIDMiddleware, accessLogMiddleware(log))
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func proxyHandler(
	log *slog.Logger,
	proxy *httputil.ReverseProxy,
	scaler *Scaler,
	tracker *ActivityTracker,
	limiter *rate.Limiter,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := requestIDFromCtx(r.Context())
		rlog := log.With("request_id", rid)

		if !limiter.Allow() {
			rlog.Warn("rate limited", "remote", r.RemoteAddr, "path", r.URL.Path)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		// Record activity before scale-up so the idle monitor never races a
		// long cold start.
		tracker.Touch()

		if err := scaler.EnsureReady(r.Context()); err != nil {
			switch {
			case errors.Is(err, context.Canceled):
				rlog.Info("client cancelled during scale-up")
				// Client closed the connection; status is largely advisory.
				w.WriteHeader(499)
			case errors.Is(err, context.DeadlineExceeded):
				rlog.Warn("scale-up exceeded ready timeout")
				http.Error(w, "Service Unavailable: scale-up timed out", http.StatusServiceUnavailable)
			default:
				rlog.Error("scale-up failed", "err", err)
				http.Error(w, "Service Unavailable: scale-up failed", http.StatusServiceUnavailable)
			}
			return
		}

		proxy.ServeHTTP(w, r)
	})
}

// --- middleware ---

type ctxKey string

const requestIDCtxKey ctxKey = "request-id"

func requestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDCtxKey).(string); ok {
		return v
	}
	return ""
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDCtxKey, id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func accessLogMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			// Skip access logs for health probes to avoid noise.
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				return
			}
			log.Info("http request",
				"request_id", requestIDFromCtx(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"remote", r.RemoteAddr,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func chainMiddleware(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// responseRecorder captures the status and bytes written for access logging.
// Flush + Unwrap are forwarded so streaming responses and Go 1.20+ http
// response control still work end-to-end.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is exceptional; fall back to a timestamp ID.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}
