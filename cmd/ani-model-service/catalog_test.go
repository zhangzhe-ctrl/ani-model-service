package main

import (
	conf "github.com/zhangzhe-ctrl/ani-model-service/api/model/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCatalogStartupRequiresIdentityConfiguration(t *testing.T) {
	t.Setenv("ANI_MODEL_CLIENT_CA", "")
	t.Setenv("ANI_MODEL_TLS_CERT", "")
	t.Setenv("ANI_MODEL_TLS_KEY", "")
	bc := &conf.Bootstrap{Server: &conf.Server{Grpc: &conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)}, Admin: &conf.Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)}, ShutdownTimeout: durationpb.New(time.Second)}}
	err := runCatalogRead(bc, newRuntimeLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("missing identity configuration accepted: %v", err)
	}
}
