package server

import (
	"context"
	"net/http"

	"github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware"
	kratoshttp "github.com/go-kratos/kratos/v3/transport/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func NewAdminServer(
	c *conf.Server_Admin,
	readiness *Readiness,
	gatherer prometheus.Gatherer,
	middlewares ...middleware.Middleware,
) *kratoshttp.Server {
	server := kratoshttp.NewServer(
		kratoshttp.Network(c.Network),
		kratoshttp.Address(c.Addr),
		kratoshttp.Timeout(c.Timeout.AsDuration()),
		kratoshttp.Middleware(middlewares...),
		kratoshttp.StrictSlash(false),
		kratoshttp.NotFoundHandler(http.NotFoundHandler()),
		kratoshttp.MethodNotAllowedHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		})),
	)
	router := server.Route("/")
	router.GET("healthz", func(ctx kratoshttp.Context) error {
		return invoke(ctx, func(context.Context, any) (any, error) {
			return map[string]string{"status": "ok"}, nil
		})
	})
	router.GET("readyz", func(ctx kratoshttp.Context) error {
		return invoke(ctx, func(context.Context, any) (any, error) {
			if !readiness.Ready() {
				return nil, errors.ServiceUnavailable("NOT_READY", "runtime is not ready")
			}
			return map[string]string{"status": "ready"}, nil
		})
	})
	metricsHandler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
	router.GET("metrics", func(ctx kratoshttp.Context) error {
		return invoke(ctx, func(context.Context, any) (any, error) {
			metricsHandler.ServeHTTP(ctx.Response(), ctx.Request())
			return nil, nil
		})
	})
	return server
}

func invoke(ctx kratoshttp.Context, handler middleware.Handler) error {
	return invokeWithRequest(ctx, nil, handler)
}

func invokeWithRequest(ctx kratoshttp.Context, request any, handler middleware.Handler) error {
	reply, err := ctx.Middleware(handler)(ctx, request)
	return ctx.Returns(reply, err)
}
