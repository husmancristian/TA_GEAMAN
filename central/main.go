package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/husmancristian/TA_GEAMAN/pkg/api"                // Adjust import path
	"github.com/husmancristian/TA_GEAMAN/pkg/config"             // Use config package
	"github.com/husmancristian/TA_GEAMAN/pkg/queue/rabbitmq"     // Use RabbitMQ queue
	"github.com/husmancristian/TA_GEAMAN/pkg/storage/persistent" // Use Persistent store
)

func main() {
	// --- Configuration Loading ---
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// --- Logger Setup ---
	logLevel := slog.LevelInfo // Default
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger) // Set as default logger for convenience

	logger.Info("Starting Test Automation Server...", slog.String("log_level", cfg.LogLevel))
	logger.Info("Configured Projects", slog.Any("projects", cfg.Projects)) // Log configured projects

	// --- Context for graceful shutdown ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop() // Call stop on exit to release resources

	// --- Dependency Injection ---
	// Initialize the queue manager (RabbitMQ)
	queueManager, err := rabbitmq.NewRabbitMQManager(cfg.RabbitMQ_URL, logger)
	if err != nil {
		logger.Error("Failed to initialize RabbitMQ queue manager", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer queueManager.Close() // Ensure connection is closed on shutdown

	// Initialize the result store (PostgreSQL + MinIO)
	resultStore, err := persistent.NewStore(
		cfg.Postgres_DSN,
		cfg.MinIO_Endpoint,
		cfg.MinIO_AccessKey,
		cfg.MinIO_SecretKey,
		cfg.MinIO_BucketName,
		cfg.MinIO_UseSSL,
		logger,
	)
	if err != nil {
		logger.Error("Failed to initialize persistent result store", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer resultStore.Close() // Ensure connections are closed

	// Create the API handler instance, injecting dependencies AND config
	apiHandler := api.NewAPI(queueManager, resultStore, logger, cfg)

	// --- Router Setup --- Pass config to router setup
	router := api.SetupRouter(apiHandler, cfg) // Pass cfg here
	logger.Info("API router configured")

	// --- HTTP Server Setup ---
	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.RequestTimeout + (5 * time.Second), // Slightly longer than handler timeout
		WriteTimeout: cfg.RequestTimeout + (5 * time.Second),
		IdleTimeout:  60 * time.Second,
		BaseContext:  func(_ net.Listener) context.Context { return ctx }, // Use app context
	}

	// --- Start Server Goroutine ---
	go func() {
		logger.Info("Server listening...", slog.String("address", server.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Server failed to start or unexpectedly closed", slog.String("error", err.Error()))
			stop() // Trigger shutdown context
		}
	}()

	// --- Wait for shutdown signal ---
	<-ctx.Done()
	logger.Info("Shutdown signal received, starting graceful shutdown...")

	// --- Graceful Shutdown ---
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second) // Timeout for shutdown
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server graceful shutdown failed", slog.String("error", err.Error()))
	} else {
		logger.Info("Server gracefully stopped")
	}

	// Dependencies are closed via defer statements earlier

	logger.Info("Shutdown complete.")
}
