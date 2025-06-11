package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// responseWriterInterceptor is a wrapper around http.ResponseWriter that captures the status code and response body.
type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

// NewResponseWriterInterceptor creates a new responseWriterInterceptor.
func newResponseWriterInterceptor(w http.ResponseWriter) *responseWriterInterceptor {
	return &responseWriterInterceptor{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default to 200
		body:           new(bytes.Buffer),
	}
}

// WriteHeader captures the status code.
func (rwi *responseWriterInterceptor) WriteHeader(statusCode int) {
	rwi.statusCode = statusCode
	rwi.ResponseWriter.WriteHeader(statusCode)
}

// Write captures the response body and calls the underlying Write.
func (rwi *responseWriterInterceptor) Write(b []byte) (int, error) {
	rwi.body.Write(b)
	return rwi.ResponseWriter.Write(b)
}

// StructuredRequestLogger is a middleware that logs request details and response body using slog.
func StructuredRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ww is a WrapResponseWriter that captures status and bytes written,
			// which is useful for standard logging metrics.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			// rwi wraps ww to capture the response body.
			rwi := newResponseWriterInterceptor(ww)

			t1 := time.Now()
			defer func() {
				requestID := middleware.GetReqID(r.Context())
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}

				logger.Info("http request",
					slog.String("request_id", requestID),
					slog.String("method", r.Method),
					slog.String("host", r.Host),
					slog.String("path", r.URL.Path),
					slog.String("proto", r.Proto),
					slog.String("scheme", scheme),
					slog.String("remote_addr", r.RemoteAddr),
					slog.String("user_agent", r.UserAgent()),
					slog.Int("status", rwi.statusCode),
					slog.Int("bytes_written", ww.BytesWritten()),
					slog.Duration("latency", time.Since(t1)),
					slog.String("response_body", rwi.body.String()), // Captured response body
				)
				println("\n")
			}()

			next.ServeHTTP(rwi, r)
		})
	}
}
