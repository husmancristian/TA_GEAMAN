package persistent

import (
	"context"
	"database/sql" // Using database/sql for broader compatibility if needed later
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"time"

	"github.com/husmancristian/TA_GEAMAN/pkg/models" // Adjust import path	"time"
	"github.com/husmancristian/TA_GEAMAN/pkg/storage"

	"github.com/jackc/pgx/v5"         // Import pgx directly for Rows handling
	"github.com/jackc/pgx/v5/pgxpool" // Using pgx pool
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Ensure Store implements storage.ResultStore interface at compile time
var _ storage.ResultStore = (*Store)(nil)

const (
	// Updated UPSERT to include new fields available at enqueue/update time
	upsertResultSQL = `
		INSERT INTO test_results (
			job_id, project, status, details, priority, enqueued_at, -- Enqueue fields
			logs, messages, duration_seconds, started_at, ended_at, -- Result fields
			screenshots, videos, metadata, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW()
		)
		ON CONFLICT (job_id) DO UPDATE SET
			project = COALESCE(EXCLUDED.project, test_results.project), -- Keep original if not provided in update
			status = EXCLUDED.status,
			details = COALESCE(EXCLUDED.details, test_results.details), -- Keep original if not provided
			priority = COALESCE(EXCLUDED.priority, test_results.priority), -- Keep original if not provided
			enqueued_at = COALESCE(EXCLUDED.enqueued_at, test_results.enqueued_at), -- Keep original if not provided
			logs = EXCLUDED.logs,
			messages = EXCLUDED.messages,
			duration_seconds = EXCLUDED.duration_seconds,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			screenshots = EXCLUDED.screenshots,
			videos = EXCLUDED.videos,
			metadata = EXCLUDED.metadata,
			updated_at = NOW()
		WHERE test_results.job_id = $1; -- Ensure WHERE clause for ON CONFLICT UPDATE
	`
	// SQL query to retrieve a test result by job_id (includes new fields).
	getResultSQL = `
		SELECT
			job_id, status, project, logs, messages, duration_seconds, started_at, ended_at,
			screenshots, videos, metadata
		FROM test_results
		WHERE job_id = $1;
	`
	// FIX: SQL query to update status and potentially started_at time with explicit cast
	updateStatusAndStartSQL = `
		UPDATE test_results
		SET status = $2,
		    -- Add explicit ::TIMESTAMPTZ cast to parameter $3
		    started_at = CASE WHEN $3::TIMESTAMPTZ IS NOT NULL THEN $3::TIMESTAMPTZ ELSE started_at END,
			updated_at = NOW()
		WHERE job_id = $1;
	`
	// Updated SQL query to retrieve ACTIVE jobs (Pending, Running, AbortRequested)
	getActiveJobsSQL = `
		SELECT
			job_id, project, status, details, priority, enqueued_at, started_at, ended_at
		FROM test_results
		WHERE status IN ($1, $2, $3) -- Filter by active statuses
		ORDER BY enqueued_at ASC -- Example: oldest pending first
		LIMIT 200; -- Example limit
	`
	// SQL query to retrieve all test results for a specific project.
	getProjectResultsSQL = `
		SELECT
			job_id, project, status, 
			messages, duration_seconds, started_at, ended_at,
			metadata
		FROM test_results
		WHERE project = $1
		ORDER BY COALESCE(started_at, ended_at) DESC; -- Show most recent first
	`

	// SQL for creating the table (Updated for reference)
	/*
		-- Run this manually or via migrations after connecting to the DB:
		CREATE TABLE IF NOT EXISTS test_results (
			job_id VARCHAR(36) PRIMARY KEY,          -- UUID
			project VARCHAR(255) NOT NULL,
			status VARCHAR(50) NOT NULL,
			details JSONB,                            -- Added
			priority INT,                             -- Added
			enqueued_at TIMESTAMPTZ,                  -- Added
			logs TEXT,
			messages TEXT[],
			duration_seconds DOUBLE PRECISION,
			started_at TIMESTAMPTZ,
			ended_at TIMESTAMPTZ,
			screenshots TEXT[],
			videos TEXT[],
			metadata JSONB,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		);
		-- Indexes (consider adding indexes on status, enqueued_at if frequently queried)
		CREATE INDEX IF NOT EXISTS idx_test_results_project ON test_results (project);
		CREATE INDEX IF NOT EXISTS idx_test_results_status ON test_results (status);
		CREATE INDEX IF NOT EXISTS idx_test_results_created_at ON test_results (created_at);
		CREATE INDEX IF NOT EXISTS idx_test_results_updated_at ON test_results (updated_at);
		CREATE INDEX IF NOT EXISTS idx_test_results_enqueued_at ON test_results (enqueued_at);
	*/

)

// Store implements the storage.ResultStore interface using PostgreSQL and MinIO.
type Store struct {
	db          *pgxpool.Pool // PostgreSQL connection pool
	minioClient *minio.Client // MinIO client
	bucketName  string        // MinIO bucket name
	logger      *slog.Logger
}

// NewStore creates a new persistent store instance.
func NewStore(pgDSN, minioEndpoint, minioAccessKey, minioSecretKey, bucketName string, useSSL bool, logger *slog.Logger) (*Store, error) {
	// --- Connect to PostgreSQL ---
	dbpool, err := pgxpool.New(context.Background(), pgDSN)
	if err != nil {
		return nil, fmt.Errorf("unable to create connection pool: %w", err)
	}
	if err := dbpool.Ping(context.Background()); err != nil {
		dbpool.Close()
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}
	logger.Info("PostgreSQL connection pool established")

	// --- Connect to MinIO ---
	minioClient, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(minioAccessKey, minioSecretKey, ""), Secure: useSSL})
	if err != nil {
		dbpool.Close()
		return nil, fmt.Errorf("failed to initialize MinIO client: %w", err)
	}
	logger.Info("MinIO client initialized", slog.String("endpoint", minioEndpoint))

	// --- Ensure MinIO Bucket Exists ---
	// Use a separate context for bucket operations for clarity, though the parent context could be used.
	bucketCtx, bucketCancel := context.WithTimeout(context.Background(), 30*time.Second) // Increased timeout slightly for bucket + policy
	defer bucketCancel()

	exists, err := minioClient.BucketExists(bucketCtx, bucketName)
	if err != nil {
		dbpool.Close() // Close DB pool before returning
		return nil, fmt.Errorf("failed to check if MinIO bucket '%s' exists: %w", bucketName, err)
	}
	if !exists {
		err = minioClient.MakeBucket(bucketCtx, bucketName, minio.MakeBucketOptions{})
		if err != nil {
			dbpool.Close() // Close DB pool before returning
			return nil, fmt.Errorf("failed to make MinIO bucket '%s': %w", bucketName, err)
		}
		logger.Info("Successfully created MinIO bucket", slog.String("bucket", bucketName))
	} else {
		logger.Info("MinIO bucket already exists", slog.String("bucket", bucketName))
	}

	// --- Set Public Read Policy for the Bucket ---
	// This policy makes all objects in the bucket publicly readable via GET requests.
	policy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {"AWS":["*"]},
				"Action": ["s3:GetObject"],
				"Resource": ["arn:aws:s3:::%s/*"]
			}
		]
	}`, bucketName)

	err = minioClient.SetBucketPolicy(bucketCtx, bucketName, policy)
	if err != nil {
		// Log a warning. Depending on your deployment, you might want to treat this as a fatal error.
		// If the policy is already set or managed externally, this error might be benign.
		logger.Warn("Failed to set public read policy on MinIO bucket. Artifacts may not be accessible via public URLs.",
			slog.String("bucket", bucketName), slog.String("error", err.Error()))
		// Example: To make it fatal:
		// dbpool.Close()
		// return nil, fmt.Errorf("failed to set public read policy on MinIO bucket '%s': %w", bucketName, err)
	} else {
		logger.Info("Successfully set public read policy on MinIO bucket", slog.String("bucket", bucketName))
	}

	return &Store{db: dbpool, minioClient: minioClient, bucketName: bucketName, logger: logger}, nil
}

// Close closes the database connection pool.
func (s *Store) Close() error {
	s.logger.Info("Closing persistent storage connections")
	if s.db != nil {
		s.db.Close()
	}
	return nil
}

// CreatePendingJob saves the initial PENDING state of a job using UPSERT.
func (s *Store) CreatePendingJob(ctx context.Context, job *models.TestJob) error {
	if job == nil || job.ID == "" || job.Project == "" {
		return fmt.Errorf("invalid job data for creating pending job")
	}

	detailsJSON, err := json.Marshal(job.Details)
	if err != nil {
		if job.Details == nil {
			detailsJSON = []byte("null")
		} else {
			return fmt.Errorf("failed to marshal job details: %w", err)
		}
	}

	// Use UPSERT: Insert PENDING state, or update if somehow exists (shouldn't happen often)
	_, err = s.db.Exec(ctx, upsertResultSQL,
		job.ID,
		job.Project,
		models.StatusPending, // Explicitly set PENDING
		detailsJSON,          // details
		nil,                  // details - let COALESCE handle it from existing or EXCLUDED if it were part of TestResult
		nil,                  // priority - let COALESCE handle it
		nil,                  // enqueued_at - let COALESCE handle it
		sql.NullString{},     // logs
		[]string(nil),        // messages (nil slice -> NULL array)
		sql.NullFloat64{},    // duration_seconds
		sql.NullTime{},       // started_at
		sql.NullTime{},       // ended_at
		[]string(nil),        // screenshots (nil slice -> NULL array)
		[]string(nil),        // videos (nil slice -> NULL array)
		[]byte("null"),       // metadata (JSON null)
	)
	if err != nil {
		return fmt.Errorf("failed to execute upsert for pending job %s: %w", job.ID, err)
	}
	s.logger.Info("Saved pending job state", slog.String("job_id", job.ID))
	return nil
}

// SaveResult UPSERTS the result metadata to PostgreSQL. Handles both initial save and updates.
func (s *Store) SaveResult(ctx context.Context, result *models.TestResult) error {

	metadataJSON, err := json.Marshal(result.Metadata)
	if err != nil {
		if result.Metadata == nil {
			metadataJSON = []byte("null")
		} else {
			return fmt.Errorf("failed to marshal result metadata: %w", err)
		}
	}
	if result.JobID == "" {
		return fmt.Errorf("cannot save result with empty JobID")
	}

	projectName := result.Project
	if projectName == "" {
		projectName = "unknown_project"
		s.logger.Warn("Project name missing in SaveResult", slog.String("job_id", result.JobID))
	}

	_, err = s.db.Exec(ctx, upsertResultSQL,
		result.JobID,
		result.Status,
		projectName, // Project needed for potential INSERT
		nil,
		nil, // priority - let COALESCE handle it
		nil, // enqueued_at - let COALESCE handle it
		sql.NullString{String: result.Logs, Valid: result.Logs != ""},
		result.Messages, // Pass slice directly, pgx handles nil/empty mapping to NULL/{}
		sql.NullFloat64{Float64: result.Duration, Valid: result.Duration > 0},
		sql.NullTime{Time: result.StartedAt, Valid: !result.StartedAt.IsZero()},
		sql.NullTime{Time: result.EndedAt, Valid: !result.EndedAt.IsZero()},
		result.Screenshots, // Pass slice directly
		result.Videos,      // Pass slice directly
		metadataJSON,
	)
	if err != nil {
		return fmt.Errorf("failed to execute upsert result query for job %s: %w", result.JobID, err)
	}
	s.logger.Info("Saved result metadata", slog.String("job_id", result.JobID), slog.String("status", result.Status))
	return nil
}

// GetResult retrieves result metadata from PostgreSQL including new fields.
func (s *Store) GetResult(ctx context.Context, jobID string) (*models.TestResult, error) {
	result := &models.TestResult{JobID: jobID}

	err := s.db.QueryRow(ctx, getResultSQL, jobID).Scan(
		&result.JobID, &result.Project, &result.Status,
		&result.Logs, &result.Messages, &result.Duration,
		&result.StartedAt, &result.EndedAt,
		&result.Screenshots, &result.Videos, &result.Metadata,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query result for job %s: %w", jobID, err)
	}

	return result, nil
}

// GetJobs retrieves a list of ACTIVE jobs (Pending, Running, AbortRequested) from PostgreSQL.
func (s *Store) GetJobs(ctx context.Context) ([]models.TestJob, error) {
	rows, err := s.db.Query(ctx, getActiveJobsSQL,
		models.StatusPending, models.StatusRunning, models.StatusAbortRequested,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query active jobs: %w", err)
	}
	defer rows.Close()

	jobs := []models.TestJob{}
	for rows.Next() {
		var job models.TestJob
		var detailsJSON []byte
		var startedAt, endedAt, enqueuedAt sql.NullTime
		var priority sql.NullInt32

		err := rows.Scan(
			&job.ID, &job.Project, &job.Status, &detailsJSON, &priority, &enqueuedAt, &startedAt, &endedAt,
		)
		if err != nil {
			s.logger.Error("Failed to scan active job row", slog.String("error", err.Error()))
			continue
		}

		job.Priority = uint8(priority.Int32)
		job.EnqueuedAt = enqueuedAt.Time
		job.StartedAt = startedAt.Time
		job.EndedAt = endedAt.Time
		if detailsJSON != nil && string(detailsJSON) != "null" {
			if err := json.Unmarshal(detailsJSON, &job.Details); err != nil {
				s.logger.Warn("Failed to unmarshal details JSON for active job", slog.String("job_id", job.ID), slog.String("error", err.Error()))
			}
		}
		jobs = append(jobs, job)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating active job rows: %w", err)
	}
	return jobs, nil
}

// GetResultsByProject retrieves all test results for a given project.
func (s *Store) GetResultsByProject(ctx context.Context, project string) ([]models.TestResult, error) {
	rows, err := s.db.Query(ctx, getProjectResultsSQL, project)

	if err != nil {
		return nil, fmt.Errorf("failed to query results for project %s: %w", project, err)
	}
	defer rows.Close()

	results := []models.TestResult{}
	for rows.Next() {
		var result models.TestResult

		// Ensure the Scan call matches the columns selected in getProjectResultsSQL
		err := rows.Scan(
			&result.JobID, &result.Project, &result.Status, &result.Messages, &result.Duration,
			&result.StartedAt, &result.EndedAt, &result.Metadata, // Assuming Metadata is the last selected field for this simplified query
		)
		// The log call was helpful for debugging, you can keep or remove it.
		// s.logger.Info("LOG", slog.String("job id ", result.JobID), slog.String("project ", result.Project), slog.String("status ", result.Status),
		// 	slog.Float64("duration ", result.Duration), slog.String("started at ", result.StartedAt.String()),
		// 	slog.String("ended at ", result.EndedAt.String()),
		// )
		if err != nil {
			s.logger.Error("Failed to scan project result row", slog.String("project", project), slog.String("error", err.Error()))
			continue // Skip this row and try to process others
		}

		results = append(results, result)

	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating project result rows for project %s: %w", project, err)
	}
	return results, nil

}

// StoreArtifact uploads data to the configured MinIO bucket.
func (s *Store) StoreArtifact(ctx context.Context, objectName string, reader io.Reader, size int64, contentType string) (string, error) {
	if s.bucketName == "" {
		return "", fmt.Errorf("minio bucket name is not configured")
	}
	uploadInfo, err := s.minioClient.PutObject(ctx, s.bucketName, objectName, reader, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", fmt.Errorf("failed to upload artifact '%s': %w", objectName, err)
	}
	s.logger.Info("Stored artifact", slog.String("bucket", uploadInfo.Bucket), slog.String("key", uploadInfo.Key), slog.Int64("size", uploadInfo.Size))
	artifactURL := url.URL{Scheme: "http", Host: s.minioClient.EndpointURL().Host, Path: path.Join(s.bucketName, objectName)}
	if opts := s.minioClient.EndpointURL(); opts.Scheme == "https" {
		artifactURL.Scheme = "https"
	}
	return artifactURL.String(), nil
}

// UpdateJobStatus updates the status and optionally the started_at time for a job.
func (s *Store) UpdateJobStatus(ctx context.Context, jobID string, status string) error {
	if jobID == "" {
		return fmt.Errorf("cannot update status for empty JobID")
	}

	// Revert to passing sql.NullTime, rely on SQL cast for type inference
	var startedAtArg sql.NullTime
	if status == models.StatusRunning {
		startedAtArg = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}
	// Otherwise, startedAtArg remains zero value (Valid=false)

	// Use the SQL query with the explicit cast ::TIMESTAMPTZ
	cmdTag, err := s.db.Exec(ctx, updateStatusAndStartSQL, jobID, status, startedAtArg)
	if err != nil {
		return fmt.Errorf("failed to execute update status query for job %s: %w", jobID, err)
	}
	if cmdTag.RowsAffected() == 0 {
		s.logger.Warn("Attempted to update status for non-existent job", slog.String("job_id", jobID), slog.String("status", status))
		return nil // Treat as non-critical if job wasn't found
	}
	s.logger.Info("Updated job status in storage", slog.String("job_id", jobID), slog.String("new_status", status))
	return nil
}
