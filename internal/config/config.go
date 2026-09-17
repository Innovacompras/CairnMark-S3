// Package config loads runtime configuration from the environment (12-factor).
// It is imported only by the composition root (cmd/server); no other package
// reads the environment directly.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	HTTPAddr        string        // listen address, e.g. ":8080"
	ShutdownTimeout time.Duration // graceful shutdown grace period
	PresignTTL      time.Duration // lifetime of presigned download URLs
	MaxUploadBytes  int64         // reject uploads larger than this; 0 disables the cap

	Postgres Postgres
	Storage  Storage
	GC       GC
	Archive  Archive
	Jobs     Jobs
}

// GC configures the background reconciliation sweep. A non-positive Interval
// disables it. GracePeriod is how old an unreferenced object must be before it
// is treated as an orphan — it must exceed the longest plausible upload so an
// in-flight write (object stored, row not yet committed) is never reclaimed.
// IdempotencyTTL is how long upload idempotency keys are retained before the
// sweep expires them (non-positive disables expiry).
type GC struct {
	Interval       time.Duration
	GracePeriod    time.Duration
	IdempotencyTTL time.Duration
}

// Postgres holds the metadata database connection settings.
type Postgres struct {
	DSN string // pgx connection string / URL
}

// Storage holds the S3-compatible backend settings.
//
// Endpoint is where the service performs all object I/O (often an internal
// network name like "minio:9000"). PublicEndpoint is the host baked into
// presigned URLs handed back to clients; it defaults to Endpoint and only
// differs when external callers reach the store by a different name than the
// service does. The host is part of the SigV4 signature, so presigned URLs are
// signed against PublicEndpoint directly — the host cannot be rewritten after.
type Storage struct {
	Endpoint       string // host:port for service-side I/O, no scheme
	PublicEndpoint string // host:port clients are redirected to; defaults to Endpoint
	Region         string
	AccessKey      string
	SecretKey      string
	Bucket         string
	UseSSL         bool // TLS for the service-side endpoint
	PublicUseSSL   bool // TLS scheme for presigned URLs; defaults to UseSSL
}

// Load reads configuration from the environment, applying defaults. It returns
// an error if a required value is missing or malformed. As of Phase 1 the
// Postgres and S3 settings are required — the server connects to both at boot.
func Load() (Config, error) {
	storage, err := loadStorage()
	if err != nil {
		return Config{}, err
	}
	archive, err := loadArchive()
	if err != nil {
		return Config{}, err
	}
	jobs, err := loadJobs()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		HTTPAddr:        getenv("CAIRNMARK_HTTP_ADDR", ":8080"),
		ShutdownTimeout: 10 * time.Second,
		PresignTTL:      15 * time.Minute,
		Postgres: Postgres{
			DSN: getenv("CAIRNMARK_POSTGRES_DSN", ""),
		},
		Storage: storage,
		Archive: archive,
		Jobs:    jobs,
		GC: GC{
			Interval:       5 * time.Minute,
			GracePeriod:    time.Hour,
			IdempotencyTTL: 24 * time.Hour,
		},
	}

	if err := getduration("CAIRNMARK_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if err := getduration("CAIRNMARK_PRESIGN_TTL", &cfg.PresignTTL); err != nil {
		return Config{}, err
	}
	if err := getduration("CAIRNMARK_GC_INTERVAL", &cfg.GC.Interval); err != nil {
		return Config{}, err
	}
	if err := getduration("CAIRNMARK_GC_GRACE_PERIOD", &cfg.GC.GracePeriod); err != nil {
		return Config{}, err
	}
	if err := getduration("CAIRNMARK_IDEMPOTENCY_TTL", &cfg.GC.IdempotencyTTL); err != nil {
		return Config{}, err
	}
	if err := getint64("CAIRNMARK_MAX_UPLOAD_BYTES", &cfg.MaxUploadBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxUploadBytes < 0 {
		return Config{}, fmt.Errorf("config: CAIRNMARK_MAX_UPLOAD_BYTES must be >= 0 (0 disables the cap), got %d", cfg.MaxUploadBytes)
	}
	if cfg.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("config: CAIRNMARK_SHUTDOWN_TIMEOUT must be positive, got %s", cfg.ShutdownTimeout)
	}
	if cfg.PresignTTL <= 0 {
		return Config{}, fmt.Errorf("config: CAIRNMARK_PRESIGN_TTL must be positive, got %s", cfg.PresignTTL)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadStorage reads the S3 settings. PublicEndpoint/PublicUseSSL default to the
// service-side endpoint and TLS scheme when not explicitly set, so a single
// endpoint config keeps working unchanged.
func loadStorage() (Storage, error) {
	endpoint := getenv("CAIRNMARK_S3_ENDPOINT", "")
	useSSL := false
	if err := getbool("CAIRNMARK_S3_USE_SSL", &useSSL); err != nil {
		return Storage{}, err
	}
	publicUseSSL := useSSL
	if err := getbool("CAIRNMARK_S3_PUBLIC_USE_SSL", &publicUseSSL); err != nil {
		return Storage{}, err
	}
	return Storage{
		Endpoint:       endpoint,
		PublicEndpoint: getenv("CAIRNMARK_S3_PUBLIC_ENDPOINT", endpoint),
		Region:         getenv("CAIRNMARK_S3_REGION", "us-east-1"),
		AccessKey:      getenv("CAIRNMARK_S3_ACCESS_KEY", ""),
		SecretKey:      getenv("CAIRNMARK_S3_SECRET_KEY", ""),
		Bucket:         getenv("CAIRNMARK_S3_BUCKET", ""),
		UseSSL:         useSSL,
		PublicUseSSL:   publicUseSSL,
	}, nil
}

// validate enforces the values the running service cannot start without.
func (c Config) validate() error {
	var missing []string
	if c.Postgres.DSN == "" {
		missing = append(missing, "CAIRNMARK_POSTGRES_DSN")
	}
	if c.Storage.Endpoint == "" {
		missing = append(missing, "CAIRNMARK_S3_ENDPOINT")
	}
	if c.Storage.AccessKey == "" {
		missing = append(missing, "CAIRNMARK_S3_ACCESS_KEY")
	}
	if c.Storage.SecretKey == "" {
		missing = append(missing, "CAIRNMARK_S3_SECRET_KEY")
	}
	if c.Storage.Bucket == "" {
		missing = append(missing, "CAIRNMARK_S3_BUCKET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: required env vars not set: %s", strings.Join(missing, ", "))
	}

	if err := validateEndpoint("CAIRNMARK_S3_ENDPOINT", c.Storage.Endpoint); err != nil {
		return err
	}
	if err := validateEndpoint("CAIRNMARK_S3_PUBLIC_ENDPOINT", c.Storage.PublicEndpoint); err != nil {
		return err
	}
	return nil
}

// validateEndpoint sanity-checks an S3 endpoint. minio-go expects a bare
// host:port, so a scheme, path, or whitespace is almost certainly a
// misconfiguration — reject it at boot rather than emit broken presigned URLs.
func validateEndpoint(name, v string) error {
	if v == "" {
		return nil
	}
	if strings.Contains(v, "://") {
		return fmt.Errorf("config: %s must be a bare host:port without a scheme, got %q", name, v)
	}
	if strings.ContainsAny(v, " /") {
		return fmt.Errorf("config: %s must be a bare host:port, got %q", name, v)
	}
	return nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// getduration overrides *dst from the env var if set, returning a wrapped error
// on a malformed value. An unset var leaves the existing default in place.
func getduration(key string, dst *time.Duration) error {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("config: %s: %w", key, err)
	}
	*dst = parsed
	return nil
}

// getint64 overrides *dst from the env var if set, returning a wrapped error on
// a malformed value. An unset var leaves the existing default in place.
func getint64(key string, dst *int64) error {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("config: %s: %w", key, err)
	}
	*dst = parsed
	return nil
}

// getbool overrides *dst from the env var if set, returning a wrapped error on
// a malformed value — a TLS toggle silently falling back to its default is a
// misconfiguration, not a preference. An unset var leaves the default in place.
func getbool(key string, dst *bool) error {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("config: %s: %w", key, err)
	}
	*dst = parsed
	return nil
}
