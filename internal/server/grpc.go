package server

import (
	"github.com/go-kratos/kratos/v3/middleware"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"

	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
)

func NewGRPCServer(c *conf.Server_GRPC, middlewares ...middleware.Middleware) *kratosgrpc.Server {
	return kratosgrpc.NewServer(
		kratosgrpc.Network(c.Network),
		kratosgrpc.Address(c.Addr),
		kratosgrpc.Timeout(c.Timeout.AsDuration()),
		kratosgrpc.Middleware(middlewares...),
		kratosgrpc.DisableReflection(),
	)
}
