package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kratostracing "github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	kratos "github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	kratoslog "github.com/go-kratos/kratos/v3/log"
	kratosmetadata "github.com/go-kratos/kratos/v3/metadata"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	grpcmetadata "google.golang.org/grpc/metadata"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	serverpkg "github.com/liangzai006/ani-model-service/internal/server"
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func TestRuntimeLifecycle(t *testing.T) {
	cfg := runtimeConfig()
	readiness := serverpkg.NewReadiness()
	var logOutput bytes.Buffer
	logger := newTestLogger(&logOutput)
	observability, err := serverpkg.NewObservability("ani-model-service-test", "test", readiness)
	if err != nil {
		t.Fatalf("NewObservability() error = %v", err)
	}
	middlewares := observability.ServerMiddleware(logger)
	grpcServer := serverpkg.NewGRPCServer(cfg.Server.Grpc, middlewares...)
	fixture := &runtimeFixture{}
	registerRuntimeFixture(grpcServer, fixture)
	adminServer := serverpkg.NewAdminServer(cfg.Server.Admin, readiness, observability.Gatherer(), middlewares...)
	grpcEndpoint, err := grpcServer.Endpoint()
	if err != nil {
		t.Fatalf("gRPC Endpoint() error = %v", err)
	}
	adminEndpoint, err := adminServer.Endpoint()
	if err != nil {
		t.Fatalf("admin Endpoint() error = %v", err)
	}

	app := kratos.New(
		kratos.Name("ani-model-service-test"),
		kratos.Logger(logger),
		kratos.Server(grpcServer, adminServer),
		kratos.AfterStart(func(context.Context) error { readiness.Set(true); return nil }),
		kratos.BeforeStop(func(context.Context) error { readiness.Set(false); return nil }),
		kratos.AfterStop(observability.Shutdown),
		kratos.StopTimeout(cfg.Server.ShutdownTimeout.AsDuration()),
	)
	runResult := make(chan error, 1)
	go func() { runResult <- app.Run() }()

	waitForReady(t, readiness)
	assertAdminEndpoint(t, adminEndpoint.Host, "/healthz", http.StatusOK, `"status":"ok"`)
	assertAdminEndpoint(t, adminEndpoint.Host, "/readyz", http.StatusOK, `"status":"ready"`)
	assertAdminEndpoint(t, adminEndpoint.Host, "/metrics", http.StatusOK, "ani_runtime_ready 1")
	assertAdminEndpoint(t, adminEndpoint.Host, "/metrics", http.StatusOK, "server_requests_code_total")
	assertGRPCHealth(t, grpcEndpoint.Host)
	assertGRPCReflectionDisabled(t, grpcEndpoint.Host)
	assertGRPCMiddleware(t, grpcEndpoint.Host, fixture)
	readiness.Set(false)
	assertAdminEndpoint(t, adminEndpoint.Host, "/readyz", http.StatusServiceUnavailable, `"reason":"NOT_READY"`)
	assertAdminEndpoint(t, adminEndpoint.Host, "/metrics", http.StatusOK, "ani_runtime_ready 0")

	if err := app.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop gracefully")
	}
	if readiness.Ready() {
		t.Fatal("runtime remained ready after stop")
	}
	if !strings.Contains(logOutput.String(), `"msg":"server request"`) {
		t.Fatalf("Kratos server logging middleware did not emit request log: %s", logOutput.String())
	}
	if !regexp.MustCompile(`"trace_id":"[0-9a-f]{32}"`).MatchString(logOutput.String()) {
		t.Fatalf("Kratos tracing middleware did not correlate request log: %s", logOutput.String())
	}
	if !regexp.MustCompile(`"span_id":"[0-9a-f]{16}"`).MatchString(logOutput.String()) {
		t.Fatalf("Kratos tracing middleware did not correlate request span: %s", logOutput.String())
	}
}

func TestAdminReadinessUsesKratosErrorEncoding(t *testing.T) {
	cfg := runtimeConfig()
	readiness := serverpkg.NewReadiness()
	logger := newTestLogger(io.Discard)
	observability, err := serverpkg.NewObservability("ani-model-service-test", "test", readiness)
	if err != nil {
		t.Fatalf("NewObservability() error = %v", err)
	}
	defer observability.Shutdown(context.Background())
	adminServer := serverpkg.NewAdminServer(
		cfg.Server.Admin,
		readiness,
		observability.Gatherer(),
		observability.ServerMiddleware(logger)...,
	)

	recorder := httptest.NewRecorder()
	adminServer.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"reason":"NOT_READY"`) {
		t.Fatalf("GET /readyz = %d %q", recorder.Code, recorder.Body.String())
	}
	metricsRecorder := httptest.NewRecorder()
	adminServer.ServeHTTP(metricsRecorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsRecorder.Code != http.StatusOK || !strings.Contains(metricsRecorder.Body.String(), "ani_runtime_ready 0") {
		t.Fatalf("GET /metrics before ready = %d %q", metricsRecorder.Code, metricsRecorder.Body.String())
	}
}

func TestCommittedConfigLoadsAsGeneratedType(t *testing.T) {
	t.Setenv("ANI_SERVER_GRPC_ADDR", "127.0.0.1:29090")
	t.Setenv("ANI_SERVER_ADMIN_ADDR", "127.0.0.1:29091")
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test path")
	}
	configPath := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "configs"))
	c := config.New(config.WithSource(file.NewSource(configPath), env.NewSource("ANI")))
	defer c.Close()
	if err := c.Load(); err != nil {
		t.Fatalf("config Load(%s) error = %v", configPath, err)
	}
	var cfg conf.Bootstrap
	if err := c.Scan(&cfg); err != nil {
		t.Fatalf("config Scan(%s) error = %v", configPath, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config Validate(%s) error = %v", configPath, err)
	}
	if cfg.Server.Grpc.Addr != "127.0.0.1:29090" || cfg.Server.Admin.Addr != "127.0.0.1:29091" {
		t.Fatalf("environment override not applied: grpc=%q admin=%q", cfg.Server.Grpc.Addr, cfg.Server.Admin.Addr)
	}
}

func TestBizLayerHasNoFrameworkOrAdapterImports(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test path")
	}
	bizRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "internal", "biz"))
	forbidden := []string{"go-kratos", "protobuf", "grpc", "internal/data", "database/sql", "pgx", "redis"}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(bizRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			for _, needle := range forbidden {
				if strings.Contains(importPath, needle) {
					t.Errorf("%s imports forbidden dependency %q", path, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan biz layer: %v", err)
	}
}

func runtimeConfig() *conf.Bootstrap {
	return &conf.Bootstrap{Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
}

func waitForReady(t *testing.T, readiness *serverpkg.Readiness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readiness.Ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime did not become ready")
}

func assertAdminEndpoint(t *testing.T, addr, path string, status int, bodyContains string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if resp.StatusCode != status || !strings.Contains(string(body), bodyContains) {
		t.Fatalf("GET %s = %d %q, want %d containing %q", path, resp.StatusCode, body, status, bodyContains)
	}
}

func assertGRPCHealth(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	defer conn.Close()
	response, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("gRPC health: %v", err)
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("gRPC health = %s", response.Status)
	}
}

func assertGRPCReflectionDisabled(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	defer conn.Close()
	stream, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return
		}
		t.Fatalf("open reflection stream: %v", err)
	}
	if err := stream.Send(&reflectionv1.ServerReflectionRequest{
		MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("send reflection request: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("reflection status = %v, want %v", status.Code(err), codes.Unimplemented)
	}
}

type runtimeFixtureServer interface {
	Check(context.Context, *conf.Bootstrap) (*emptypb.Empty, error)
}

type runtimeFixture struct {
	calls atomic.Int32
}

func (f *runtimeFixture) Check(ctx context.Context, _ *conf.Bootstrap) (*emptypb.Empty, error) {
	md, ok := kratosmetadata.FromServerContext(ctx)
	if !ok || md.Get("x-md-layout-caller") != "runtime-test" {
		return nil, errors.New("Kratos metadata middleware did not propagate x-md-layout-caller")
	}
	if md.Get("x-md-layout-panic") == "true" {
		panic("gRPC recovery middleware test")
	}
	f.calls.Add(1)
	return &emptypb.Empty{}, nil
}

func registerRuntimeFixture(server *kratosgrpc.Server, fixture runtimeFixtureServer) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "ani.layout.test.RuntimeFixture",
		HandlerType: (*runtimeFixtureServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Check",
			Handler: func(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				request := new(conf.Bootstrap)
				if err := decode(request); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return service.(runtimeFixtureServer).Check(ctx, request)
				}
				info := &grpc.UnaryServerInfo{Server: service, FullMethod: "/ani.layout.test.RuntimeFixture/Check"}
				handler := func(ctx context.Context, request any) (any, error) {
					return service.(runtimeFixtureServer).Check(ctx, request.(*conf.Bootstrap))
				}
				return interceptor(ctx, request, info, handler)
			},
		}},
	}, fixture)
}

func assertGRPCMiddleware(t *testing.T, addr string, fixture *runtimeFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC middleware fixture: %v", err)
	}
	defer connection.Close()

	valid := &conf.Bootstrap{Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(time.Second),
	}}
	metadataContext := grpcmetadata.AppendToOutgoingContext(ctx, "x-md-layout-caller", "runtime-test")
	if err := connection.Invoke(metadataContext, "/ani.layout.test.RuntimeFixture/Check", valid, &emptypb.Empty{}); err != nil {
		t.Fatalf("metadata middleware request: %v", err)
	}
	if fixture.calls.Load() != 1 {
		t.Fatalf("fixture calls after valid request = %d", fixture.calls.Load())
	}

	if err := connection.Invoke(metadataContext, "/ani.layout.test.RuntimeFixture/Check", &conf.Bootstrap{}, &emptypb.Empty{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("validation middleware status = %v, want %v", status.Code(err), codes.InvalidArgument)
	}
	if fixture.calls.Load() != 1 {
		t.Fatal("validation middleware called the handler")
	}

	panicContext := grpcmetadata.AppendToOutgoingContext(metadataContext, "x-md-layout-panic", "true")
	if err := connection.Invoke(panicContext, "/ani.layout.test.RuntimeFixture/Check", valid, &emptypb.Empty{}); status.Code(err) != codes.Internal {
		t.Fatalf("recovery middleware status = %v, want %v", status.Code(err), codes.Internal)
	}
}

func newTestLogger(writer io.Writer) *slog.Logger {
	return slog.New(kratoslog.NewHandler(
		kratoslog.WithWriter(writer),
		kratoslog.WithFormat(kratoslog.FormatJSON),
		kratoslog.WithExtractor(kratostracing.TraceAttrs),
		kratoslog.WithFilter(kratoslog.FilterKey("args")),
	))
}
