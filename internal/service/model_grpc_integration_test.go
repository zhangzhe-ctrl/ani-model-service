package service

import (
	"context"
	"net"
	"testing"

	"github.com/liangzai006/ani-model-service/internal/identity"
	modelv1 "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestModelGRPCCrossTenantRequestIsDenied(t *testing.T) {
	service := NewModelCatalogService(nil, &catalogFake{})
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		return next(identity.WithPrincipal(ctx, identity.Principal{TenantID: "tenant-a", Actor: "gateway", Workload: "gateway"}), req)
	}))
	modelv1.RegisterModelServiceServer(server, service)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := modelv1.NewModelServiceClient(conn)
	_, err = client.CreateModel(context.Background(), &modelv1.CreateModelRequest{TenantId: "tenant-b", Name: "cross-tenant"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status=%v err=%v, want PermissionDenied", status.Code(err), err)
	}
}
