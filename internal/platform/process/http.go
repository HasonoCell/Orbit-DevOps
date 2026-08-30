package process

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

func ServeHTTP(
	ctx context.Context,
	server *http.Server,
	logger *slog.Logger,
) error {
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
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
