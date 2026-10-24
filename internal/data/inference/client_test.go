package inference

import (
	"context"
	"errors"
	"net"
	"testing"

	inferencev1 "github.com/liangzai006/ani-inference-service/api/inference/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type referenceServer struct {
	inferencev1.UnimplementedModelReferenceServiceServer
	tenant string
	ids    []string
	fail   bool
}

func (s *referenceServer) CheckModelVersionReferences(ctx context.Context, req *inferencev1.CheckModelVersionReferencesRequest) (*inferencev1.CheckModelVersionReferencesResponse, error) {
	if s.fail {
		return nil, status.Error(codes.Unavailable, "database unavailable")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	s.tenant = md.Get("x-ani-tenant-id")[0]
	s.ids = req.ModelVersionIds
	return &inferencev1.CheckModelVersionReferencesResponse{HasActiveReferences: true}, nil
}
func TestReferenceClientGRPCAndFailure(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	handler := &referenceServer{}
	inferencev1.RegisterModelReferenceServiceServer(server, handler)
	go server.Serve(lis)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///inference", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := NewClient(inferencev1.NewModelReferenceServiceClient(conn), "tenant-a")
	ids := []string{"22222222-2222-4222-8222-222222222222"}
	active, err := client.HasActiveVersionReferences(context.Background(), "tenant-a", ids)
	if err != nil || !active || handler.tenant != "tenant-a" || len(handler.ids) != 1 || handler.ids[0] != ids[0] {
		t.Fatalf("active=%v err=%v", active, err)
	}
	if _, err = client.HasActiveVersionReferences(context.Background(), "tenant-b", ids); err == nil {
		t.Fatal("cross-tenant credential accepted")
	}
	handler.fail = true
	if active, err = client.HasActiveVersionReferences(context.Background(), "tenant-a", ids); err == nil || active {
		t.Fatalf("dependency failure lost active=%v err=%v", active, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.HasActiveVersionReferences(ctx, "tenant-a", ids)
	if !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
}
