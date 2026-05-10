package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration sourced from environment variables.
// Defaults are applied where reasonable; required fields are validated in
// validate().
type Config struct {
	// Worker scaling target.
	DeploymentName string
	Namespace      string
	TargetURL      string

	// HTTP server.
	ListenAddr      string
	LogLevel        string
	ShutdownTimeout time.Duration

	// Scale-to-zero behaviour.
	IdleTimeout       time.Duration
	IdleCheckInterval time.Duration

	// Cold-start behaviour.
	ReadyTimeout time.Duration
	PollInterval time.Duration

	// Rate limiting (per gateway pod).
	RateLimitRPS   float64
	RateLimitBurst int

	// Optional upstream TLS — used when the worker pod terminates TLS via a
	// Caddy sidecar of its own. Leave empty to disable.
	UpstreamCACert             string
	UpstreamClientCert         string
	UpstreamClientKey          string
	UpstreamServerName         string
	UpstreamInsecureSkipVerify bool
}

// LoadConfig reads configuration from the environment and returns it after
// validation. Any parse error is wrapped with the offending key.
func LoadConfig() (*Config, error) {
	c := &Config{
		DeploymentName: getEnv("DEPLOYMENT_NAME", "ollama-worker"),
		Namespace:      getEnv("NAMESPACE", "default"),
		TargetURL:      getEnv("OLLAMA_SERVICE_URL", "https://ollama-worker-svc:443"),

		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		LogLevel:   getEnv("LOG_LEVEL", "info"),

		UpstreamCACert:     getEnv("UPSTREAM_CA_CERT", ""),
		UpstreamClientCert: getEnv("UPSTREAM_CLIENT_CERT", ""),
		UpstreamClientKey:  getEnv("UPSTREAM_CLIENT_KEY", ""),
		UpstreamServerName: getEnv("UPSTREAM_SERVER_NAME", ""),
	}

	var err error
	if c.IdleTimeout, err = parseDurationEnv("IDLE_TIMEOUT", "10m"); err != nil {
		return nil, err
	}
	if c.IdleCheckInterval, err = parseDurationEnv("IDLE_CHECK_INTERVAL", "30s"); err != nil {
		return nil, err
	}
	if c.ReadyTimeout, err = parseDurationEnv("READY_TIMEOUT", "2m"); err != nil {
		return nil, err
	}
	if c.PollInterval, err = parseDurationEnv("POLL_INTERVAL", "2s"); err != nil {
		return nil, err
	}
	if c.ShutdownTimeout, err = parseDurationEnv("SHUTDOWN_TIMEOUT", "30s"); err != nil {
		return nil, err
	}
	if c.RateLimitRPS, err = parseFloatEnv("RATE_LIMIT_RPS", "5"); err != nil {
		return nil, err
	}
	if c.RateLimitBurst, err = parseIntEnv("RATE_LIMIT_BURST", "10"); err != nil {
		return nil, err
	}
	if c.UpstreamInsecureSkipVerify, err = parseBoolEnv("UPSTREAM_INSECURE_SKIP_VERIFY", "false"); err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DeploymentName == "" {
		return errors.New("DEPLOYMENT_NAME must be set")
	}
	if c.Namespace == "" {
		return errors.New("NAMESPACE must be set")
	}
	if c.TargetURL == "" {
		return errors.New("OLLAMA_SERVICE_URL must be set")
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("IDLE_TIMEOUT must be positive, got %s", c.IdleTimeout)
	}
	if c.IdleCheckInterval <= 0 {
		return fmt.Errorf("IDLE_CHECK_INTERVAL must be positive, got %s", c.IdleCheckInterval)
	}
	if c.IdleCheckInterval >= c.IdleTimeout {
		return fmt.Errorf("IDLE_CHECK_INTERVAL (%s) must be < IDLE_TIMEOUT (%s)", c.IdleCheckInterval, c.IdleTimeout)
	}
	if c.ReadyTimeout <= 0 {
		return fmt.Errorf("READY_TIMEOUT must be positive, got %s", c.ReadyTimeout)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("POLL_INTERVAL must be positive, got %s", c.PollInterval)
	}
	if c.PollInterval >= c.ReadyTimeout {
		return fmt.Errorf("POLL_INTERVAL (%s) must be < READY_TIMEOUT (%s)", c.PollInterval, c.ReadyTimeout)
	}
	if c.RateLimitRPS <= 0 {
		return fmt.Errorf("RATE_LIMIT_RPS must be positive, got %v", c.RateLimitRPS)
	}
	if c.RateLimitBurst <= 0 {
		return fmt.Errorf("RATE_LIMIT_BURST must be positive, got %d", c.RateLimitBurst)
	}
	if (c.UpstreamClientCert == "") != (c.UpstreamClientKey == "") {
		return errors.New("UPSTREAM_CLIENT_CERT and UPSTREAM_CLIENT_KEY must both be set or both be empty")
	}
	if c.UpstreamInsecureSkipVerify && (c.UpstreamCACert != "" || c.UpstreamClientCert != "") {
		return errors.New("UPSTREAM_INSECURE_SKIP_VERIFY cannot be combined with UPSTREAM_CA_CERT or UPSTREAM_CLIENT_CERT")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseDurationEnv(key, fallback string) (time.Duration, error) {
	raw := getEnv(key, fallback)
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, raw, err)
	}
	return d, nil
}

func parseFloatEnv(key, fallback string) (float64, error) {
	raw := getEnv(key, fallback)
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, raw, err)
	}
	return f, nil
}

func parseIntEnv(key, fallback string) (int, error) {
	raw := getEnv(key, fallback)
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, raw, err)
	}
	return n, nil
}

func parseBoolEnv(key, fallback string) (bool, error) {
	raw := getEnv(key, fallback)
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s=%q: %w", key, raw, err)
	}
	return b, nil
}
