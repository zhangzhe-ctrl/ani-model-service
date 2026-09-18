package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	"github.com/jackc/pgx/v5/pgxpool"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/data/postgres"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-model-service/internal/service"
)

func runCatalogRead(bc *conf.Bootstrap, logger *slog.Logger) error {
	if err := bc.Validate(); err != nil {
		return err
	}
	tlsConfig, err := server.GovernanceTLS(os.Getenv("ANI_MODEL_CLIENT_CA"), os.Getenv("ANI_MODEL_TLS_CERT"), os.Getenv("ANI_MODEL_TLS_KEY"))
	if err != nil {
		return err
	}
	dsn := os.Getenv("ANI_DATABASE_DSN")
	if dsn == "" {
		return fmt.Errorf("catalog-read requires ANI_DATABASE_DSN")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("catalog-read PostgreSQL config: %w", err)
	}
	// Bound reconnect attempts below the upstream request budget. An established
	// slow query still follows its request context; an unreachable DB returns 503.
	if poolConfig.ConnConfig.ConnectTimeout <= 0 || poolConfig.ConnConfig.ConnectTimeout > time.Second {
		poolConfig.ConnConfig.ConnectTimeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err == nil {
		err = pool.Ping(ctx)
	}
	cancel()
	if pool != nil {
		defer pool.Close()
	}
	if err != nil {
		return fmt.Errorf("catalog-read PostgreSQL: %w", err)
	}
	readiness := server.NewCatalogReadiness(pool.Ping)
	obs, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		return err
	}
	middlewares := obs.ServerMiddleware(logger)
	gs := kratosgrpc.NewServer(
		kratosgrpc.Network(bc.Server.Grpc.Network), kratosgrpc.Address(bc.Server.Grpc.Addr),
		kratosgrpc.Timeout(bc.Server.Grpc.Timeout.AsDuration()), kratosgrpc.TLSConfig(tlsConfig),
		kratosgrpc.UnaryInterceptor(server.GovernanceUnary()), kratosgrpc.StreamInterceptor(server.DenyCatalogStreams),
		kratosgrpc.DisableReflection(), kratosgrpc.Middleware(middlewares...),
	)
	catalog := postgres.NewCatalogStore(postgres.NewModelStore(pool))
	conf.RegisterModelServiceServer(gs, service.NewModelServiceWithDependencies(nil, nil, catalog, nil, nil))
	admin := server.NewAdminServer(bc.Server.Admin, readiness, obs.Gatherer(), middlewares...)
	return newApp(logger, gs, admin, readiness, obs, nil, bc.Server.ShutdownTimeout.AsDuration()).Run()
}
