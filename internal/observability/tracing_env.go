package observability

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// validateOTLPEnvironment 先拒绝异常运行配置，避免官方 exporter 解析时打印原始 Header 或文件名。
// 本轮 TLS 使用系统信任链；自定义 CA/mTLS 不属于当前配置契约。
func validateOTLPEnvironment() error {
	invalid := errors.New("invalid OTLP runtime configuration")
	for _, prefix := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_TRACES_"} {
		if raw := os.Getenv(prefix + "ENDPOINT"); raw != "" {
			config := DefaultTraceConfig()
			config.Exporter, config.Endpoint = "otlp", raw
			if config.Validate() != nil {
				return invalid
			}
		}
		for _, pair := range strings.Split(os.Getenv(prefix+"HEADERS"), ",") {
			if pair == "" {
				continue
			}
			key, value, found := strings.Cut(pair, "=")
			decoded, err := url.PathUnescape(value)
			if !found || err != nil || !httpguts.ValidHeaderFieldName(strings.TrimSpace(key)) || !httpguts.ValidHeaderFieldValue(decoded) {
				return invalid
			}
		}
		if raw := os.Getenv(prefix + "TIMEOUT"); raw != "" {
			if value, err := strconv.ParseInt(raw, 10, 64); err != nil || value <= 0 {
				return invalid
			}
		}
		if raw := os.Getenv(prefix + "INSECURE"); raw != "" {
			if _, err := strconv.ParseBool(raw); err != nil {
				return invalid
			}
		}
		for _, suffix := range []string{"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
			if os.Getenv(prefix+suffix) != "" {
				return errors.New("custom OTLP TLS material is not supported")
			}
		}
	}
	return nil
}
