package main

import (
	"context"
	"log/slog"
	"time"

	kratos "github.com/go-kratos/kratos/v3"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	kratoshttp "github.com/go-kratos/kratos/v3/transport/http"

	"github.com/liangzai006/ani-model-service/internal/server"
	"github.com/liangzai006/ani-model-service/internal/service"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func buildApp(bc *conf.Bootstrap, logger *slog.Logger) (*kratos.App, error) {
	return buildAppWithModelService(bc, logger, service.NewModelService(nil), false, false, false)
}

func buildAppWithModelService(bc *conf.Bootstrap, logger *slog.Logger, modelService *service.ModelService, dependencyGated bool, postgresReady, storageReady bool, workerRunners ...server.WorkerRunner) (*kratos.App, error) {
	if err := bc.Validate(); err != nil {
		return nil, err
	}
	readiness := server.NewReadiness()
	if dependencyGated {
		readiness.RequireDependencies()
		readiness.SetPostgresReady(postgresReady)
		readiness.SetStorageReady(storageReady)
	}
	observability, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		return nil, err
	}
	// Admin HTTP is only /healthz, /readyz, and /metrics. kubelet probes cannot
	// present a client certificate, so they must not pass through RPC authorization.
	adminMiddleware := observability.ServerMiddleware(logger)
	grpcMiddleware := append(adminMiddleware, RPCPolicyMiddleware(ModelServicePolicy()), ModelPrincipalMiddleware())

	grpcServer, err := configuredModelGRPCServer(bc.Server.Grpc, grpcMiddleware...)
	if err != nil {
		_ = observability.Shutdown(context.Background())
		return nil, err
	}
	conf.RegisterModelServiceServer(grpcServer, modelService)
	adminServer := server.NewAdminServer(bc.Server.Admin, readiness, observability.Gatherer(), adminMiddleware...)
	var supervisor *server.WorkerSupervisor
	if len(workerRunners) > 0 && workerRunners[0] != nil {
		supervisor = server.NewWorkerSupervisor(workerRunners[0], readiness)
	}
	return newApp(logger, grpcServer, adminServer, readiness, observability, supervisor, bc.Server.ShutdownTimeout.AsDuration()), nil
}

func newApp(
	logger *slog.Logger,
	grpcServer *kratosgrpc.Server,
	adminServer *kratoshttp.Server,
	readiness *server.Readiness,
	observability *server.Observability,
	workerSupervisor *server.WorkerSupervisor,
	stopTimeout time.Duration,
) *kratos.App {
	return kratos.New(
		kratos.ID(id),
		kratos.Name(Name),
		kratos.Version(Version),
		kratos.Logger(logger),
		kratos.Server(grpcServer, adminServer),
		kratos.AfterStart(func(ctx context.Context) error {
			if workerSupervisor != nil {
				if err := workerSupervisor.Start(ctx); err != nil {
					return err
				}
			}
			readiness.Set(true)
			return nil
		}),
		kratos.BeforeStop(func(ctx context.Context) error {
			readiness.Set(false)
			if workerSupervisor != nil {
				return workerSupervisor.Stop(ctx)
			}
			return nil
		}),
		kratos.AfterStop(observability.Shutdown),
		kratos.StopTimeout(stopTimeout),
	)
}
