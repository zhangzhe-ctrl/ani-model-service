package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/go-kratos/kratos/v3/log"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	bizstorage "github.com/liangzai006/ani-model-service/internal/biz/storage"
	"github.com/liangzai006/ani-model-service/internal/data/importer"
	kubedata "github.com/liangzai006/ani-model-service/internal/data/kubernetes"
	"github.com/liangzai006/ani-model-service/internal/data/postgres"
	storagedata "github.com/liangzai006/ani-model-service/internal/data/storage"
	"github.com/liangzai006/ani-model-service/internal/server"
	"github.com/liangzai006/ani-model-service/internal/service"
	"github.com/liangzai006/ani-model-service/internal/worker"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Name and Version can be overridden with -ldflags at build time.
var (
	Name     = "ani-model-service"
	Version  = "dev"
	flagconf string
	id, _    = os.Hostname()
)

type minioReadinessChecker interface {
	CheckBucket(context.Context) error
	CheckTenantConnection(context.Context, string) error
}

func checkMinIOStorage(ctx context.Context, storage minioReadinessChecker, tenantBuckets bool) error {
	if tenantBuckets {
		// Tenant buckets are created lazily after a tenant is known. Check
		// credentials and endpoint without creating a bucket.
		return storage.CheckTenantConnection(ctx, uuid.NewString())
	}
	return storage.CheckBucket(ctx)
}

func init() {
	flag.StringVar(&flagconf, "conf", "configs", "config path, for example -conf configs/config.yaml")
}

func main() {
	flag.Parse()
	logger := newRuntimeLogger(os.Stdout)
	log.SetDefault(logger)

	// Validate all required configuration early, before any resource initialization
	if err := validateRequiredConfig(); err != nil {
		logger.Error("configuration validation failed", "error", err)
		os.Exit(1)
	}

	if err := run(logger); err != nil {
		logger.Error("service terminated", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	undoMaxProcs, err := maxprocs.Set(maxprocs.Logger(func(format string, args ...interface{}) {
		log.Info("runtime CPU quota", "detail", fmt.Sprintf(format, args...))
	}))
	if err != nil {
		return fmt.Errorf("configure runtime CPU quota: %w", err)
	}
	defer undoMaxProcs()

	c := config.New(config.WithSource(
		file.NewSource(flagconf),
		env.NewSource("ANI"),
	))
	defer c.Close()
	if err := c.Load(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		return fmt.Errorf("scan config: %w", err)
	}
	modelService := service.NewModelService(nil)
	// Use one provider registry for both transient catalog search and the
	// existing import worker so provider URL and HTTP behavior stay aligned.
	providerHTTPClient := newImporterHTTPClient()
	providers := importer.Registry{
		"huggingface": importer.NewHuggingFaceAdapter(os.Getenv("ANI_HUGGINGFACE_BASE_URL"), providerHTTPClient),
		"modelscope":  importer.NewModelScopeAdapter(os.Getenv("ANI_MODELSCOPE_BASE_URL"), providerHTTPClient),
	}
	var importWorker *worker.Worker
	var storagePort bizstorage.Port
	var storageConn *grpc.ClientConn
	storageReady := false
	minioEndpoint := strings.TrimSpace(os.Getenv("ANI_MINIO_ENDPOINT"))
	minioSecure := os.Getenv("ANI_MINIO_SECURE") == "true"
	minioTenantBuckets := strings.EqualFold(strings.TrimSpace(os.Getenv("ANI_MINIO_TENANT_BUCKETS")), "true")
	minioBucket := firstEnv("ANI_MINIO_BUCKET", "ani-models")
	grpcEndpoint := strings.TrimSpace(os.Getenv("ANI_STORAGE_GRPC_ADDR"))
	if minioEndpoint != "" && grpcEndpoint != "" {
		return fmt.Errorf("configure only one of ANI_MINIO_ENDPOINT and ANI_STORAGE_GRPC_ADDR")
	}
	if minioEndpoint != "" {
		var minioStorage *storagedata.MinIOAdapter
		var createErr error
		if minioTenantBuckets {
			minioStorage, createErr = storagedata.NewMinIOTenantBucketAdapter(minioEndpoint, os.Getenv("ANI_MINIO_ACCESS_KEY"), os.Getenv("ANI_MINIO_SECRET_KEY"), minioSecure)
		} else {
			minioStorage, createErr = storagedata.NewMinIOAdapter(minioEndpoint, os.Getenv("ANI_MINIO_ACCESS_KEY"), os.Getenv("ANI_MINIO_SECRET_KEY"), minioBucket, minioSecure)
		}
		err = createErr
		if err != nil {
			return fmt.Errorf("configure MinIO Storage: %w", err)
		}
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = checkMinIOStorage(checkCtx, minioStorage, minioTenantBuckets)
		cancel()
		if err != nil {
			return fmt.Errorf("check MinIO Storage: %w", err)
		}
		storagePort = minioStorage
		storageReady = true
	}
	if grpcEndpoint != "" {
		tlsConfig, tlsErr := buildStorageTLSConfig()
		if tlsErr != nil {
			return fmt.Errorf("configure Storage TLS: %w", tlsErr)
		}
		dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		opts := []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		}
		storagePort, storageConn, err = storagedata.DialGRPC(dialCtx, grpcEndpoint, opts...)
		cancel()
		if err != nil {
			return fmt.Errorf("connect Storage gRPC: %w", err)
		}
		defer storageConn.Close()
		storageReady = true
	}
	var pool *pgxpool.Pool
	postgresReady := false
	if dsn := os.Getenv("ANI_DATABASE_DSN"); dsn != "" {
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err = pgxpool.New(pingCtx, dsn)
		cancel()
		if err != nil {
			return fmt.Errorf("connect PostgreSQL: %w", err)
		}
		// Ensure pool is closed on any subsequent error
		defer pool.Close()

		pingCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		err = pool.Ping(pingCtx2)
		cancel2()
		if err != nil {
			return fmt.Errorf("ping PostgreSQL: %w", err)
		}
		postgresReady = true
		modelStore := postgres.NewModelStore(pool)
		versionStore := postgres.NewVersionStore(pool)
		workStore := postgres.NewWorkStore(pool)
		modelService = service.NewModelServiceWithDependencies(versionStore, versionStore, postgres.NewCatalogStore(modelStore), storagePort, workStore)
		modelService.SetImportTaskReader(workStore)
		modelService.SetImportTaskRetrier(workStore)
		modelService.SetArtifactStore(postgres.NewArtifactStore(pool))
		modelService.SetAuditStore(postgres.NewAuditStore(pool))
		modelService.SetBundleImporter(postgres.NewBundleImportStore(pool))
		modelService.SetTrainedArtifactRegistrar(postgres.NewTrainedArtifactStore(pool))
		if storagePort != nil {
			owner := strings.TrimSpace(os.Getenv("ANI_IMPORT_WORKER_OWNER"))
			if owner == "" {
				owner = id
			}

			artifactStore := postgres.NewArtifactStore(pool)
			executor, configErr := configuredKubernetesImportExecutor(storagePort)
			if configErr != nil {
				return configErr
			}
			kubeConfig, configErr := rest.InClusterConfig()
			if configErr != nil {
				return fmt.Errorf("configure Kubernetes import Job: %w", configErr)
			}
			kubeClient, clientErr := k8sclient.NewForConfig(kubeConfig)
			if clientErr != nil {
				return fmt.Errorf("create Kubernetes import client: %w", clientErr)
			}
			executor.Jobs = kubedata.NewImportJobClient(kubeClient)
			modelService.SetImportTaskLogReader(executor.Jobs, executor.Namespace)
			executor.Artifacts = artifactStore
			executor.Checksums = versionStore
			executor.Providers = providers
			importWorker = &worker.Worker{
				Store: workStore, Binder: postgres.NewImportBinder(pool, providers), Executor: executor,
				Finalizer: versionStore, TenantID: "", Owner: owner, Logger: logger,
			}
		}
	}
	// Attach the shared provider registry after the database-backed service is
	// constructed; that branch replaces the initial service value.
	modelService.SetExternalModelSearcher(providers)
	var runners []server.WorkerRunner
	references, closeReferences, err := configuredInferenceReferences()
	if err != nil {
		return fmt.Errorf("configure Inference references: %w", err)
	}
	defer closeReferences()
	modelService.SetInferenceReferenceChecker(references)
	if importWorker != nil {
		modelService.SetImportNotifier(importWorker)
		runners = append(runners, importWorker)
	}
	app, err := buildAppWithModelService(&bc, logger, modelService, true, postgresReady, storageReady, runners...)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	if err := app.Run(); err != nil {
		return fmt.Errorf("run app: %w", err)
	}
	return nil
}

func firstEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func configuredKubernetesImportExecutor(storagePort bizstorage.Port) (worker.KubernetesImportExecutor, error) {
	image := strings.TrimSpace(os.Getenv("ANI_IMPORT_JOB_IMAGE"))
	if image == "" {
		return worker.KubernetesImportExecutor{}, fmt.Errorf("ANI_IMPORT_JOB_IMAGE is required")
	}
	return worker.KubernetesImportExecutor{
		Storage:            storagePort,
		Namespace:          firstEnv("ANI_IMPORT_KUBERNETES_NAMESPACE", "ani-model"),
		Image:              image,
		ProviderSecretName: strings.TrimSpace(os.Getenv("ANI_IMPORT_PROVIDER_SECRET")),
		StorageClass:       strings.TrimSpace(os.Getenv("ANI_IMPORT_STORAGE_CLASS")),
		PollInterval:       time.Second,
	}, nil
}

func newRuntimeLogger(writer io.Writer) *slog.Logger {
	handler := log.NewHandler(
		log.WithWriter(writer),
		log.WithFormat(log.FormatJSON),
		log.WithLevel(log.LevelInfo),
		log.WithAddSource(true),
		log.WithExtractor(tracing.TraceAttrs),
		log.WithFilter(log.FilterKey(
			"args",
			"authorization",
			"cookie",
			"credential",
			"password",
			"private_key",
			"set-cookie",
			"token",
		)),
	)
	return slog.New(handler).With(
		slog.String("service.id", id),
		slog.String("service.name", Name),
		slog.String("service.version", Version),
	)
}

// buildStorageTLSConfig creates a TLS configuration for Storage gRPC connection.
// It supports three modes:
// 1. System cert pool (default)
// 2. Custom CA cert from file (ANI_STORAGE_GRPC_CA_CERT)
// 3. Custom CA cert from environment variable (ANI_STORAGE_GRPC_CA_CERT_PEM)
func buildStorageTLSConfig() (*tls.Config, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: os.Getenv("ANI_STORAGE_GRPC_SERVER_NAME"),
	}

	// Try to load custom CA certificate
	caCertPath := os.Getenv("ANI_STORAGE_GRPC_CA_CERT")
	caCertPEM := os.Getenv("ANI_STORAGE_GRPC_CA_CERT_PEM")

	if caCertPath != "" {
		// Load CA from file (Kubernetes Secret mount)
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("load CA cert from %s: %w", caCertPath, err)
		}
		rootCAs := x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("invalid CA certificate in %s", caCertPath)
		}
		config.RootCAs = rootCAs
		return config, nil
	}

	if caCertPEM != "" {
		// Load CA from environment variable (inline PEM)
		rootCAs := x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM([]byte(caCertPEM)) {
			return nil, fmt.Errorf("invalid CA certificate in ANI_STORAGE_GRPC_CA_CERT_PEM")
		}
		config.RootCAs = rootCAs
		return config, nil
	}

	// Use system cert pool (default)
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		// Fallback to empty pool if system pool is unavailable
		rootCAs = x509.NewCertPool()
	}
	config.RootCAs = rootCAs

	return config, nil
}

// newImporterHTTPClient creates an HTTP client with proper timeouts for external
// provider APIs (HuggingFace, ModelScope). This prevents goroutine leaks and
// service hangs when providers are slow or unresponsive.
func newImporterHTTPClient() *http.Client {
	// Parse timeout from environment variable or use default
	timeout := 15 * time.Minute
	if val := os.Getenv("ANI_IMPORTER_HTTP_TIMEOUT"); val != "" {
		if d, err := time.ParseDuration(val); err == nil && d > 0 {
			timeout = d
		}
	}

	dialTimeout := 30 * time.Second
	if val := os.Getenv("ANI_IMPORTER_DIAL_TIMEOUT"); val != "" {
		if d, err := time.ParseDuration(val); err == nil && d > 0 {
			dialTimeout = d
		}
	}

	return &http.Client{
		// Overall request timeout (including large model downloads)
		Timeout: timeout,
		Transport: &http.Transport{
			// TCP connection establishment timeout
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// TLS handshake timeout
			TLSHandshakeTimeout: 10 * time.Second,
			// Timeout waiting for response headers
			ResponseHeaderTimeout: 30 * time.Second,
			// Expect: 100-continue timeout
			ExpectContinueTimeout: 1 * time.Second,
			// Connection pool settings
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// validateRequiredConfig validates all required environment variables before
// any resource initialization. This provides fast feedback and prevents partial
// initialization when configuration is incomplete.
func validateRequiredConfig() error {
	var missing []string

	// Database is always required
	if strings.TrimSpace(os.Getenv("ANI_DATABASE_DSN")) == "" {
		missing = append(missing, "ANI_DATABASE_DSN")
	}

	// Storage backend (at least one is required)
	hasMinIO := strings.TrimSpace(os.Getenv("ANI_MINIO_ENDPOINT")) != ""
	hasStorageGRPC := strings.TrimSpace(os.Getenv("ANI_STORAGE_GRPC_ADDR")) != ""

	if !hasMinIO && !hasStorageGRPC {
		missing = append(missing, "ANI_MINIO_ENDPOINT or ANI_STORAGE_GRPC_ADDR")
	}

	// MinIO-specific direct adapter configuration
	if hasMinIO {
		if strings.TrimSpace(os.Getenv("ANI_MINIO_ACCESS_KEY")) == "" {
			missing = append(missing, "ANI_MINIO_ACCESS_KEY")
		}
		if strings.TrimSpace(os.Getenv("ANI_MINIO_SECRET_KEY")) == "" {
			missing = append(missing, "ANI_MINIO_SECRET_KEY")
		}
	}

	// Storage gRPC specific configuration
	if hasStorageGRPC {
		if strings.TrimSpace(os.Getenv("ANI_STORAGE_GRPC_SERVER_NAME")) == "" {
			missing = append(missing, "ANI_STORAGE_GRPC_SERVER_NAME")
		}
	}

	if strings.TrimSpace(os.Getenv("ANI_IMPORT_JOB_IMAGE")) == "" {
		missing = append(missing, "ANI_IMPORT_JOB_IMAGE")
	}
	if strings.TrimSpace(os.Getenv("ANI_IMPORT_KUBERNETES_NAMESPACE")) == "" {
		missing = append(missing, "ANI_IMPORT_KUBERNETES_NAMESPACE")
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return nil
}
