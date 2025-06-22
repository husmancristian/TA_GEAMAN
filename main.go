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
	"github.com/joho/godotenv"                                   // Import godotenv
)

func main() {

	// --- Logger Setup ---
	logLevel := slog.LevelInfo // Default
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger) // Set as default logger for convenience

	// --- Load .env file (for local development only) ---
	// Only attempt to load a .env file if APP_ENV is not 'production'.
	// This keeps container logs clean.
	if os.Getenv("APP_ENV") != "production" {
		if err := godotenv.Load(); err != nil {
			logger.Info("Could not load .env file, relying on environment variables", slog.String("error", err.Error()))
		} else {
			logger.Info("Loaded configuration from .env file for local development")
		}
	}

	// --- Configuration Loading ---
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Explicitly check if the PostgreSQL DSN is loaded correctly
	if cfg.Postgres_DSN == "" {
		logger.Error("PostgreSQL DSN (POSTGRES_DSN) is empty in configuration. Please ensure it's set correctly in .env or environment variables.", slog.String("dsn_from_config", cfg.Postgres_DSN))
		os.Exit(1) // Exit early if DSN is not set, as database connection will fail
	}

	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}

	logger.Info("Starting Test Automation Server...", slog.String("log_level", cfg.LogLevel))
	// logger.Info("Configured Projects", slog.Any("projects", cfg.Projects)) // Log configured projects

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

	// Certificate paths now come from your configuration
	certFile := cfg.CertFile
	keyFile := cfg.KeyFile

	// --- Start Server Goroutine ---
	go func() {
		if certFile == "" || keyFile == "" {
			slog.Error("CERT_FILE and KEY_FILE environment variables must be set for HTTPS")
			stop() // Trigger shutdown
			return
		}
		logger.Info("Server starting on address", "protocol", "https", "address", server.Addr)
		if err := server.ListenAndServeTLS(certFile, keyFile); errors.Is(err, syscall.EADDRINUSE) {
			logger.Error("Port is already in use. Is another instance of the server (or the Docker container) already running?", slog.String("address", server.Addr))
		} else if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTPS Server failed to start or unexpectedly closed", slog.String("error", err.Error()))
			// Fallback or alternative: logger.Info("Attempting to start HTTP server as HTTPS failed...")
			// if errHttp := server.ListenAndServe(); errHttp != nil && !errors.Is(errHttp, http.ErrServerClosed) { ... }
			stop() // Trigger shutdown context if HTTPS fails
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
