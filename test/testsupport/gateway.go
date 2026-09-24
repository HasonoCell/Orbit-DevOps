package testsupport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// AwaitHTTPResponse 等待本地 Gateway 数据面响应，避免把控制器短暂收敛延迟当作失败。
func AwaitHTTPResponse(t *testing.T, client *http.Client, url, hostname string, status int, bodyFragment string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = hostname
		response, err := client.Do(request)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == status && strings.Contains(string(body), bodyFragment) {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s host=%s did not return %d with body containing %q", url, hostname, status, bodyFragment)
}
