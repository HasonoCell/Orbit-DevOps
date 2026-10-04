package process

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

func ServeHTTP(
	ctx context.Context,
	server *http.Server,
	logger *slog.Logger,
) error {
	// Shutdown 超时只说明 HTTP 排空窗口结束，不说明 Handler 已返回。Close 取消连接后
	// 仍等待已登记请求，避免装配入口提前关闭它们正在使用的数据库。
	handler := server.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	var handlers sync.WaitGroup
	var mu sync.Mutex
	closing := false
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if closing {
			mu.Unlock()
			http.Error(w, "server_stopping", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		mu.Unlock()
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	})
	defer func() { mu.Lock(); closing = true; mu.Unlock(); handlers.Wait() }()
	result := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务启动", "address", server.Addr)
		result <- server.ListenAndServe()
	}()

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
