package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/go-kratos/kratos/v3/log"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/jackc/pgx/v5/pgxpool"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	bizstorage "github.com/zhangzhe-ctrl/ani-model-service/internal/biz/storage"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/data/importer"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/data/postgres"
	storagedata "github.com/zhangzhe-ctrl/ani-model-service/internal/data/storage"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/service"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Name and Version can be overridden with -ldflags at build time.
var (
	Name     = "ani-model-service"
	Version  = "dev"
	flagconf string
	id, _    = os.Hostname()
)

func init() {
	flag.StringVar(&flagconf, "conf", "configs", "config path, for example -conf configs/config.yaml")
}

func main() {
	flag.Parse()
	logger := newRuntimeLogger(os.Stdout)
	log.SetDefault(logger)
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
	mode := strings.TrimSpace(os.Getenv("ANI_MODEL_MODE"))
	if mode == "catalog-read" {
		return runCatalogRead(&bc, logger)
	}
	if mode != "" {
		return fmt.Errorf("unsupported ANI_MODEL_MODE %q", mode)
	}
	modelService := service.NewModelService(nil)
	var importWorker *worker.Worker
	var storagePort bizstorage.Port
	var storageConn *grpc.ClientConn
	storageReady := false
	minioEndpoint := strings.TrimSpace(os.Getenv("ANI_MINIO_ENDPOINT"))
	grpcEndpoint := strings.TrimSpace(os.Getenv("ANI_STORAGE_GRPC_ADDR"))
	if minioEndpoint != "" && grpcEndpoint != "" {
		return fmt.Errorf("configure only one of ANI_MINIO_ENDPOINT and ANI_STORAGE_GRPC_ADDR")
	}
	if minioEndpoint != "" {
		secure := os.Getenv("ANI_MINIO_SECURE") == "true"
		tenantBuckets := strings.EqualFold(strings.TrimSpace(os.Getenv("ANI_MINIO_TENANT_BUCKETS")), "true")
		var minioStorage *storagedata.MinIOAdapter
		var createErr error
		if tenantBuckets {
			minioStorage, createErr = storagedata.NewMinIOTenantBucketAdapter(minioEndpoint, os.Getenv("ANI_MINIO_ACCESS_KEY"), os.Getenv("ANI_MINIO_SECRET_KEY"), secure)
		} else {
			bucket := strings.TrimSpace(os.Getenv("ANI_MINIO_BUCKET"))
			if bucket == "" {
				bucket = "ani-models"
			}
			minioStorage, createErr = storagedata.NewMinIOAdapter(minioEndpoint, os.Getenv("ANI_MINIO_ACCESS_KEY"), os.Getenv("ANI_MINIO_SECRET_KEY"), bucket, secure)
		}
		err = createErr
		if err != nil {
			return fmt.Errorf("configure MinIO Storage: %w", err)
		}
		if !tenantBuckets {
			checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = minioStorage.CheckBucket(checkCtx)
			cancel()
			if err != nil {
				return fmt.Errorf("check MinIO bucket: %w", err)
			}
		} else if tenantID := strings.TrimSpace(os.Getenv("ANI_IMPORT_WORKER_TENANT_ID")); tenantID != "" {
			checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = minioStorage.CheckTenantConnection(checkCtx, tenantID)
			cancel()
			if err != nil {
				return fmt.Errorf("check MinIO tenant access: %w", err)
			}
		}
		storagePort = minioStorage
		storageReady = true
	}
	if grpcEndpoint != "" {
		dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		opts := []grpc.DialOption{grpc.WithBlock()}
		if os.Getenv("ANI_STORAGE_GRPC_INSECURE") == "true" {
			opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		} else {
			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ServerName: os.Getenv("ANI_STORAGE_GRPC_SERVER_NAME")})))
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
		if err == nil {
			err = pool.Ping(pingCtx)
		}
		cancel()
		if err != nil {
			if pool != nil {
				pool.Close()
			}
			return fmt.Errorf("connect PostgreSQL: %w", err)
		}
		defer pool.Close()
		postgresReady = true
		modelStore := postgres.NewModelStore(pool)
		versionStore := postgres.NewVersionStore(pool)
		workStore := postgres.NewWorkStore(pool)
		modelService = service.NewModelServiceWithDependencies(versionStore, versionStore, postgres.NewCatalogStore(modelStore), storagePort, workStore)
		modelService.SetArtifactStore(postgres.NewArtifactStore(pool))
		modelService.SetAuditStore(postgres.NewAuditStore(pool))
		if storagePort != nil {
			if tenantID := strings.TrimSpace(os.Getenv("ANI_IMPORT_WORKER_TENANT_ID")); tenantID != "" {
				owner := strings.TrimSpace(os.Getenv("ANI_IMPORT_WORKER_OWNER"))
				if owner == "" {
					owner = id
				}
				providers := importer.Registry{
					"huggingface": importer.NewHuggingFaceAdapter(os.Getenv("ANI_HUGGINGFACE_BASE_URL"), nil),
					"modelscope":  importer.NewModelScopeAdapter(os.Getenv("ANI_MODELSCOPE_BASE_URL"), nil),
				}
				importWorker = &worker.Worker{
					Store: workStore, Binder: postgres.NewImportBinder(pool, providers), Executor: worker.ImportExecutor{
						Providers: providers,
						Storage:   storagePort,
						Artifacts: postgres.NewArtifactStore(pool),
						Checksums: versionStore,
						Logger:    logger,
					},
					Finalizer: versionStore, TenantID: tenantID, Owner: owner, Logger: logger,
				}
			}
		}
	}
	var runners []server.WorkerRunner
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
