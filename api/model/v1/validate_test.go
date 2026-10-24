package modelv1

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func validConfig() *Bootstrap {
	return &Bootstrap{Server: &Server{
		Grpc:            &Server_GRPC{Network: "tcp", Addr: "127.0.0.1:19090", Timeout: durationpb.New(time.Second)},
		Admin:           &Server_Admin{Network: "tcp", Addr: "127.0.0.1:19091", Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(5 * time.Second),
	}}
}

func TestBootstrapValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Bootstrap)
		ok     bool
	}{
		{name: "loopback listeners", ok: true},
		{name: "unspecified pod listeners", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "0.0.0.0:19090"
			c.Server.Admin.Addr = "[::]:19091"
		}, ok: true},
		{name: "IPv6 loopback listeners", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "[::1]:19090"
			c.Server.Admin.Addr = "[::1]:19091"
		}, ok: true},
		{name: "invalid grpc network", mutate: func(c *Bootstrap) { c.Server.Grpc.Network = "udp" }},
		{name: "invalid admin network", mutate: func(c *Bootstrap) { c.Server.Admin.Network = "unix" }},
		{name: "hostname is not a literal boundary", mutate: func(c *Bootstrap) { c.Server.Admin.Addr = "localhost:19091" }},
		{name: "specific external address", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "192.0.2.1:19090" }},
		{name: "malformed address", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1" }},
		{name: "invalid port", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1:70000" }},
		{name: "zero port", mutate: func(c *Bootstrap) { c.Server.Grpc.Addr = "127.0.0.1:0" }},
		{name: "duplicate fixed address", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "127.0.0.1:19090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "wildcard conflicts with loopback port", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "0.0.0.0:19090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "equivalent numeric ports", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Addr = "127.0.0.1:019090"
			c.Server.Admin.Addr = "127.0.0.1:19090"
		}},
		{name: "missing grpc timeout", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = nil }},
		{name: "zero grpc timeout", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(0) }},
		{name: "negative admin timeout", mutate: func(c *Bootstrap) { c.Server.Admin.Timeout = durationpb.New(-time.Second) }},
		{name: "original grpc timeout", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(30 * time.Second) }, ok: true},
		{name: "one hour model facts budget", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(3600 * time.Second) }, ok: true},
		{name: "finite grpc maximum", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(24 * time.Hour) }, ok: true},
		{name: "grpc timeout above maximum", mutate: func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(24*time.Hour + time.Nanosecond) }},
		{name: "admin timeout above unchanged maximum", mutate: func(c *Bootstrap) { c.Server.Admin.Timeout = durationpb.New(31 * time.Second) }},
		{name: "invalid protobuf timeout", mutate: func(c *Bootstrap) {
			c.Server.Grpc.Timeout = &durationpb.Duration{Nanos: 1_000_000_000}
		}},
		{name: "missing shutdown timeout", mutate: func(c *Bootstrap) { c.Server.ShutdownTimeout = nil }},
		{name: "zero shutdown timeout", mutate: func(c *Bootstrap) { c.Server.ShutdownTimeout = durationpb.New(0) }},
		{name: "shutdown timeout above maximum", mutate: func(c *Bootstrap) {
			c.Server.ShutdownTimeout = durationpb.New(31 * time.Second)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := proto.Clone(validConfig()).(*Bootstrap)
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			err := cfg.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestBootstrapValidateRequiresCompleteServerConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Bootstrap
	}{
		{name: "nil bootstrap"},
		{name: "missing server", cfg: &Bootstrap{}},
		{name: "missing grpc", cfg: &Bootstrap{Server: &Server{Admin: validConfig().Server.Admin}}},
		{name: "missing admin", cfg: &Bootstrap{Server: &Server{Grpc: validConfig().Server.Grpc}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}
