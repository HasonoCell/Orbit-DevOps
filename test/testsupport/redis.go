// Package testsupport 提供仅供测试使用的独占外部依赖，不连接或清空开发者已有实例。
package testsupport

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const RedisImage = "redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf"

// StartRedis 创建该测试独占的 Redis，并在测试结束后清理此容器。
func StartRedis(t *testing.T) (testcontainers.Container, string) {
	t.Helper()
	ctx := context.Background()
	// 动态 Docker HostPort 在 Stop/Start 后可能变化。先分配空闲端口并固定绑定，
	// 重启验收才是在相同服务地址恢复，而不是悄悄切换了测试依赖地址。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, hostPort, _ := net.SplitHostPort(listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: RedisImage, ExposedPorts: []string{"6379/tcp"},
			HostConfigModifier: func(config *dockercontainer.HostConfig) {
				config.PortBindings = network.PortMap{network.MustParsePort("6379/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: hostPort}}}
			},
			WaitingFor: wait.ForListeningPort("6379/tcp").WithStartupTimeout(30 * time.Second),
		}, Started: true,
	})
	if err != nil {
		t.Fatalf("start isolated Redis: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate isolated Redis: %v", err)
		}
	})
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return container, net.JoinHostPort(host, port.Port())
}
