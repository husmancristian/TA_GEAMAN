package config

import (
	"os"
	"strconv" // Import strings package
	"time"
)

// Config holds application configuration values.
type Config struct {
	Port             string
	RabbitMQ_URL     string
	Postgres_DSN     string
	MinIO_Endpoint   string
	MinIO_AccessKey  string
	MinIO_SecretKey  string
	MinIO_UseSSL     bool
	MinIO_BucketName string
	LogLevel         string // e.g., "debug", "info", "warn", "error"
	RequestTimeout   time.Duration
	// Projects         []string // REMOVED: Projects will be managed via DB and cached in API
}

// Load loads configuration from environment variables.
func Load() (*Config, error) {
	// Helper to get env var with default
	getenv := func(key, fallback string) string {
		if value, exists := os.LookupEnv(key); exists {
			return value
		}
		return fallback
	}

	// Helper to get bool env var
	getenvBool := func(key string, fallback bool) bool {
		if valueStr, exists := os.LookupEnv(key); exists {
			value, err := strconv.ParseBool(valueStr)
			if err == nil {
				return value
			}
		}
		return fallback
	}

	// Helper to get duration env var
	getenvDuration := func(key string, fallback time.Duration) time.Duration {
		if valueStr, exists := os.LookupEnv(key); exists {
			value, err := time.ParseDuration(valueStr)
			if err == nil {
				return value
			}
		}
		return fallback
	}

	cfg := &Config{
		Port:             getenv("PORT", "8080"),
		RabbitMQ_URL:     getenv("RABBITMQ_URL", "amqp://localhost:5672/"),                                    // Fallback without credentials
		Postgres_DSN:     getenv("POSTGRES_DSN", "postgres://localhost:5432/test_results_db?sslmode=disable"), // Fallback without credentials
		MinIO_Endpoint:   getenv("MINIO_ENDPOINT", "localhost:9000"),
		MinIO_AccessKey:  getenv("MINIO_ACCESS_KEY", ""), // Fallback to empty, must be set in .env
		MinIO_SecretKey:  getenv("MINIO_SECRET_KEY", ""), // Fallback to empty, must be set in .env
		MinIO_UseSSL:     getenvBool("MINIO_USE_SSL", false),
		MinIO_BucketName: getenv("MINIO_BUCKET_NAME", "test-artifacts"),
		LogLevel:         getenv("LOG_LEVEL", "info"),
		RequestTimeout:   getenvDuration("REQUEST_TIMEOUT", 15*time.Second),
		// Projects are no longer loaded from env. They are managed via DB.
	}

	// Add validation if needed (e.g., check required fields)

	return cfg, nil
}
