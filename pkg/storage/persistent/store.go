package persistent

import (
	"context"
	"database/sql" // Using database/sql for broader compatibility if needed later
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
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
			logs, messages, duration_seconds, started_at, ended_at, passrate, progress, -- Result fields
			screenshots, videos, metadata, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, NOW()
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
			passrate = EXCLUDED.passrate,
			progress = EXCLUDED.progress,
			screenshots = EXCLUDED.screenshots,
			videos = EXCLUDED.videos,
			metadata = EXCLUDED.metadata,
			updated_at = NOW()
		WHERE test_results.job_id = $1; -- Ensure WHERE clause for ON CONFLICT UPDATE
	`
	// SQL query to retrieve a test result by job_id (includes new fields).
	getResultSQL = `
		SELECT
			job_id, project, status, details, priority, enqueued_at,
			logs, messages, duration_seconds, started_at, ended_at, passrate, progress, screenshots, videos, metadata
		FROM test_results
		WHERE job_id = $1;
	`
	// FIX: SQL query to update status and potentially started_at time with explicit cast
	updateStatusAndStartSQL = `
		UPDATE test_results
		SET status = $2,
		    started_at = CASE WHEN $3::TIMESTAMPTZ IS NOT NULL THEN $3::TIMESTAMPTZ ELSE started_at END,
			details = CASE WHEN $4::JSONB IS NOT NULL THEN $4::JSONB ELSE details END,
			updated_at = NOW()
		WHERE job_id = $1; -- $2: status, $3: started_at, $4: details
	`
	// SQL query to update job progress (status, append logs, merge metadata)
	updateJobProgressSQL = `
		UPDATE test_results
		SET
			status = CASE WHEN $2::VARCHAR IS NOT NULL THEN $2::VARCHAR ELSE status END, -- $2 is status string (sql.NullString), explicit cast
			logs = CASE WHEN $3::TEXT IS NOT NULL THEN COALESCE(logs, '') || $3::TEXT ELSE logs END, -- $3 is logs_chunk string (sql.NullString), explicit cast
			messages = CASE WHEN $4::TEXT[] IS NOT NULL THEN COALESCE(messages, '{}'::TEXT[]) || $4::TEXT[] ELSE messages END, -- $4 is messages_chunk TEXT[], explicit cast
			passrate = CASE WHEN $5::TEXT IS NOT NULL THEN $5::TEXT ELSE passrate END, -- $5 is passrate string (sql.NullString)
			progress = CASE WHEN $6::TEXT IS NOT NULL THEN $6::TEXT ELSE progress END, -- $6 is progress string (sql.NullString)
			updated_at = NOW()
		WHERE job_id = $1;
	`
	// Updated SQL query to retrieve ACTIVE jobs (Pending, Running, AbortRequested)
	getActiveJobsSQL = `
		SELECT
			job_id, project, status, details, priority, enqueued_at, started_at, ended_at, passrate, progress
		FROM test_results
		WHERE status IN ($1, $2, $3) -- Filter by active statuses
		ORDER BY enqueued_at ASC -- Example: oldest pending first
		LIMIT 200; -- Example limit
	`
	// SQL query to retrieve all test results for a specific project.
	getProjectResultsSQL = `
		SELECT
			job_id, project, status, details, ended_at, passrate, progress, duration_seconds
		FROM test_results
		WHERE project = $1
		ORDER BY COALESCE(ended_at, created_at) DESC; -- Show most recent first, fallback to created_at
	`
	// SQL query to count jobs by project and status.
	countJobsByStatusSQL = `
		SELECT COUNT(*)
		FROM test_results
		WHERE project = $1 AND status = $2;
	`

	// SQL query to get pending jobs count, running suites count, and the highest priority (lowest number) among PENDING jobs for a project.
	getProjectQueueOverviewSQL = `
		SELECT
			COALESCE(SUM(CASE WHEN status = $2 THEN 1 ELSE 0 END), 0) AS pending_jobs_count,      -- $2 = 'PENDING'
			MIN(CASE WHEN status = $2 THEN priority END) AS highest_priority,                   -- Only consider priority for PENDING jobs
			COALESCE(SUM(CASE WHEN status = $3 THEN 1 ELSE 0 END), 0) AS running_suites_count   -- $3 = 'RUNNING'
		FROM test_results
		WHERE project = $1 AND status IN ($2, $3); -- $1 = project_name
	`

	// --- Project Management SQL ---
	addProjectSQL = `
		INSERT INTO projects (name) VALUES ($1)
		ON CONFLICT (name) DO NOTHING;
	`
	deleteProjectSQL = `
		DELETE FROM projects WHERE name = $1;
	`
	getProjectsSQL = `
		SELECT name
		FROM projects
		ORDER BY name ASC;
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
			passrate TEXT,
			progress TEXT,
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

		-- Table for managing projects dynamically
		CREATE TABLE IF NOT EXISTS projects (
			name VARCHAR(255) PRIMARY KEY,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
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

// AddProject adds a new project name to the projects table.
// It uses ON CONFLICT DO NOTHING to be idempotent.
func (s *Store) AddProject(ctx context.Context, projectName string) error {
	if projectName == "" {
		return errors.New("project name cannot be empty")
	}
	_, err := s.db.Exec(ctx, addProjectSQL, projectName)
	if err != nil {
		s.logger.Error("Failed to add project to database", slog.String("project_name", projectName), slog.String("error", err.Error()))
		return fmt.Errorf("failed to execute add project query for '%s': %w", projectName, err)
	}
	s.logger.Info("Attempted to add project to database", slog.String("project_name", projectName))
	return nil
}

// DeleteProject removes a project name from the projects table.
func (s *Store) DeleteProject(ctx context.Context, projectName string) error {
	if projectName == "" {
		return errors.New("project name cannot be empty for deletion")
	}
	cmdTag, err := s.db.Exec(ctx, deleteProjectSQL, projectName)
	if err != nil {
		s.logger.Error("Failed to delete project from database", slog.String("project_name", projectName), slog.String("error", err.Error()))
		return fmt.Errorf("failed to execute delete project query for '%s': %w", projectName, err)
	}
	if cmdTag.RowsAffected() == 0 {
		s.logger.Warn("Attempted to delete a non-existent project or no rows affected", slog.String("project_name", projectName))
		// Depending on desired behavior, you might return a specific "not found" error here.
		// For now, no error if not found, as the state (project not existing) is achieved.
	} else {
		s.logger.Info("Deleted project from database", slog.String("project_name", projectName))
	}
	return nil
}

// GetProjects retrieves a list of all project names from the projects table.
func (s *Store) GetProjects(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, getProjectsSQL)
	if err != nil {
		s.logger.Error("Failed to query projects from database", slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to execute get projects query: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			s.logger.Error("Failed to scan project name row", slog.String("error", err.Error()))
			// Decide if one bad row should fail the whole operation or just be skipped.
			// For now, let's be strict and return the error.
			return nil, fmt.Errorf("failed to scan project name: %w", err)
		}
		projects = append(projects, name)
	}

	if err = rows.Err(); err != nil {
		s.logger.Error("Error iterating project rows", slog.String("error", err.Error()))
		return nil, fmt.Errorf("error iterating project rows: %w", err)
	}

	s.logger.Debug("Retrieved projects from database", slog.Int("count", len(projects)))
	return projects, nil
}

// NewStore creates a new persistent store instance.
func NewStore(pgDSN, minioEndpoint, minioPublicEndpointStr, minioAccessKey, minioSecretKey, bucketName string, useSSL bool, logger *slog.Logger) (*Store, error) {
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
	// Determine which endpoint to use for the client. We want to SIGN with the public
	// endpoint but CONNECT to the internal one.
	endpointForClientConfig := minioEndpoint
	if minioPublicEndpointStr != "" {
		endpointForClientConfig = minioPublicEndpointStr
	}

	// The minio.New() function requires an endpoint without a scheme (e.g., "localhost:9000").
	// We must strip "http://" or "https://" if it exists.
	endpointForMinioClient := endpointForClientConfig
	if i := strings.Index(endpointForMinioClient, "://"); i != -1 {
		endpointForMinioClient = endpointForMinioClient[i+3:]
	}

	// Create a custom transport that routes requests for the public host
	// to the internal Docker network host.
	customTransport := http.DefaultTransport.(*http.Transport).Clone()
	customTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// `addr` will be the host:port that the client tries to dial, which is `endpointForMinioClient`.
		publicHost := endpointForMinioClient // Already stripped of scheme.

		// If the address to dial is the public-facing one, we intercept and
		// replace it with the internal Docker network address.
		if addr == publicHost {
			addr = minioEndpoint
			logger.Debug("Redirecting MinIO connection", "from", publicHost, "to", addr)
		}

		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		return dialer.DialContext(ctx, network, addr)
	}

	// Initialize MinIO client with the public-facing endpoint for signing,
	// but using our custom transport for connecting.
	minioClient, err := minio.New(endpointForMinioClient, &minio.Options{
		Creds:     credentials.NewStaticV4(minioAccessKey, minioSecretKey, ""),
		Secure:    useSSL,
		Transport: customTransport,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MinIO client: %w", err)
	} else {
		logger.Info("MinIO client initialized",
			slog.String("signing_endpoint", endpointForMinioClient),
			slog.String("connect_endpoint", minioEndpoint),
		)
	}

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
		// Treat failure to set the bucket policy as a fatal error during startup.
		// If artifacts are not publicly readable, a core feature is broken.
		dbpool.Close()
		return nil, fmt.Errorf("failed to set public read policy on MinIO bucket '%s': %w", bucketName, err)
	} else {
		logger.Info("Successfully set public read policy on MinIO bucket", slog.String("bucket", bucketName))
	}

	return &Store{
		db:          dbpool,
		minioClient: minioClient,
		bucketName:  bucketName,
		logger:      logger,
	}, nil
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

	var logs, messages sql.NullString
	var duration sql.NullFloat64
	var startedAt, endedAt sql.NullTime

	detailsJSON, err := json.Marshal(job.Details)
	if err != nil {
		if job.Details == nil {
			detailsJSON = []byte("null")
		} else {
			return fmt.Errorf("failed to marshal job details: %w", err)
		}
	}
	enqueuedAtToStore := job.EnqueuedAt
	if enqueuedAtToStore.IsZero() {
		enqueuedAtToStore = time.Now().UTC()
	}
	// Use UPSERT: Insert PENDING state, or update if somehow exists (shouldn't happen often)
	_, err = s.db.Exec(ctx, upsertResultSQL,
		job.ID,
		job.Project,
		models.StatusPending,         // Explicitly set PENDING
		detailsJSON,                  // details
		job.Priority,                 // priority
		enqueuedAtToStore,            // enqueued at
		&logs,                        // Scan into sql.NullString
		&messages,                    // Scan into sql.NullString
		&duration,                    // Scan into sql.NullFloat64
		&startedAt,                   // Scan into sql.NullTime
		&endedAt,                     // Scan into sql.NullTime
		[]string(nil),                // screenshots (nil slice -> NULL array)
		[]string(nil),                // videos (nil slice -> NULL array)
		sql.NullString{Valid: false}, // passrate
		sql.NullString{Valid: false}, // progress
		[]byte("null"),               // metadata (JSON null)
	)
	if err != nil {
		return fmt.Errorf("failed to execute upsert for pending job %s: %w", job.ID, err)
	}
	s.logger.Info("Saved pending job state", slog.String("job_id", job.ID))
	return nil
}

// SaveResult UPSERTS the result metadata to PostgreSQL. Handles both initial save and updates.
func (s *Store) SaveResult(ctx context.Context, result *models.TestResult) error {

	var metadataToStore []byte
	// If compiler sees result.Metadata as map[string]interface{}, we need to marshal it.
	if result.Metadata != nil && len(result.Metadata) > 0 { // Check if map is not nil and not empty
		marshaledMetadata, err := json.Marshal(result.Metadata)
		if err != nil {
			s.logger.Error("Failed to marshal result metadata", slog.String("job_id", result.JobID), slog.String("error", err.Error()))
			return fmt.Errorf("failed to marshal result metadata: %w", err)
		}
		metadataToStore = marshaledMetadata
	} else {
		metadataToStore = []byte("null") // Store JSON null if input is nil, empty, or explicitly "null"
	}

	if result.JobID == "" {
		return fmt.Errorf("cannot save result with empty JobID")
	}

	projectName := result.Project
	if projectName == "" {
		projectName = "unknown_project"
		s.logger.Warn("Project name missing in SaveResult", slog.String("job_id", result.JobID))
	}

	_, err := s.db.Exec(ctx, upsertResultSQL,
		result.JobID,
		projectName, // Project needed for potential INSERT
		result.Status,
		nil,
		nil, // priority - let COALESCE handle it
		nil, // enqueued_at - let COALESCE handle it
		sql.NullString{String: result.Logs, Valid: result.Logs != ""},
		result.Messages, // Pass slice directly, pgx handles nil/empty mapping to NULL/{}
		sql.NullFloat64{Float64: result.Duration, Valid: result.Duration > 0},
		sql.NullTime{Time: result.StartedAt, Valid: !result.StartedAt.IsZero()},
		sql.NullTime{Time: result.EndedAt, Valid: !result.EndedAt.IsZero()},
		sql.NullString{String: result.Passrate, Valid: result.Passrate != ""},
		sql.NullString{String: result.Progress, Valid: result.Progress != ""},
		result.Screenshots, // Pass slice directly
		result.Videos,      // Pass slice directly
		metadataToStore,
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

	var logs sql.NullString
	var duration sql.NullFloat64
	var startedAt, endedAt, enqueuedAt sql.NullTime
	var detailsJSON []byte
	var priority sql.NullInt32
	var metadataJSON []byte // Scan JSONB into []byte
	var passrate, progress sql.NullString
	// Assuming result.Messages, result.Screenshots, result.Videos are []string
	// and result.Metadata is json.RawMessage or []byte, which pgx handles for NULL arrays/JSON.

	err := s.db.QueryRow(ctx, getResultSQL, jobID).Scan(
		&result.JobID, &result.Project, &result.Status,
		&detailsJSON, // Scan details
		&priority,    // Scan priority
		&enqueuedAt,  // Scan enqueued_at
		&logs,        // Scan into sql.NullString
		&result.Messages,
		&duration,  // Scan into sql.NullFloat64
		&startedAt, // Scan into sql.NullTime
		&endedAt,   // Scan into sql.NullTime
		&passrate, &progress, &result.Screenshots, &result.Videos, &metadataJSON,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // Job not found, return nil result and nil error
		}
		return nil, fmt.Errorf("failed to query result for job %s: %w", jobID, err)
	}

	// Populate the result struct from scanned values
	if logs.Valid {
		result.Logs = logs.String
	}
	if duration.Valid {
		result.Duration = duration.Float64
	}

	if priority.Valid {
		result.Priority = uint8(priority.Int32)
	}

	result.EnqueuedAt = enqueuedAt.Time
	result.StartedAt = startedAt.Time // If NullTime.Valid is false, Time is zero value, which is fine
	result.EndedAt = endedAt.Time     // If NullTime.Valid is false, Time is zero value, which is fine

	if detailsJSON != nil && string(detailsJSON) != "null" {
		if errUnmarshal := json.Unmarshal(detailsJSON, &result.Details); errUnmarshal != nil {
			s.logger.Warn("Failed to unmarshal details JSON for job result", slog.String("job_id", jobID), slog.String("error", errUnmarshal.Error()))
			// Potentially return error or partial result here, for now, details will be nil/empty
		}
	}
	if metadataJSON != nil && string(metadataJSON) != "null" {
		// Unmarshal into map[string]interface{}
		if errUnmarshal := json.Unmarshal(metadataJSON, &result.Metadata); errUnmarshal != nil {
			s.logger.Warn("Failed to unmarshal metadata JSON into map[string]interface{}",
				slog.String("job_id", jobID),
				slog.String("error", errUnmarshal.Error()))
		}
	}
	if passrate.Valid {
		result.Passrate = passrate.String
	}
	if progress.Valid {
		result.Progress = progress.String
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
		var startedAt, endedAt sql.NullTime
		var passrate, progress sql.NullString
		var priority sql.NullInt32

		err := rows.Scan(
			&job.ID, &job.Project, &job.Status, &detailsJSON, &priority, &job.EnqueuedAt, &startedAt, &endedAt, &passrate, &progress,
		)

		if err != nil {
			s.logger.Error("Failed to scan active job row", slog.String("error", err.Error()))
			continue
		}
		enqueuedAtToStore := job.EnqueuedAt
		if enqueuedAtToStore.IsZero() {
			enqueuedAtToStore = time.Now().UTC()
		}
		job.Priority = uint8(priority.Int32)
		job.EnqueuedAt = enqueuedAtToStore
		job.StartedAt = startedAt.Time
		job.EndedAt = endedAt.Time
		if passrate.Valid {
			job.Passrate = passrate.String
		}
		if progress.Valid {
			job.Progress = progress.String
		}
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
func (s *Store) GetResultsByProject(ctx context.Context, project string) ([]models.ProjectResultSummary, error) {
	rows, err := s.db.Query(ctx, getProjectResultsSQL, project)

	if err != nil {
		return nil, fmt.Errorf("failed to query results for project %s: %w", project, err)
	}
	defer rows.Close()
	results := []models.ProjectResultSummary{}
	for rows.Next() {
		var result models.ProjectResultSummary
		var detailsJSON []byte // For scanning details JSONB
		var passrate, progress sql.NullString
		var duration sql.NullFloat64
		var endedAt sql.NullTime // ended_at can be NULL
		// Ensure the Scan call matches the columns selected in getProjectResultsSQL
		err := rows.Scan(
			&result.JobID,
			&result.Project,
			&result.Status,
			&detailsJSON,
			&endedAt, // Correct order for ended_at (TIMESTAMPTZ)
			&passrate, &progress, &duration,
		)
		if endedAt.Valid {
			result.EndedAt = endedAt.Time
		}

		if detailsJSON != nil && string(detailsJSON) != "null" {
			if errUnmarshal := json.Unmarshal(detailsJSON, &result.Details); errUnmarshal != nil {
				s.logger.Warn("Failed to unmarshal details JSON for project result summary", slog.String("job_id", result.JobID), slog.String("error", errUnmarshal.Error()))
				// result.Details will be nil
			}
		}
		if passrate.Valid {
			result.Passrate = passrate.String
		}
		if progress.Valid {
			result.Progress = progress.String
		}
		if duration.Valid {
			result.Duration = duration.Float64
		}
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
func (s *Store) StoreArtifact(ctx context.Context, objectName string, reader io.Reader, size int64, contentType string) error {
	if s.bucketName == "" {
		return fmt.Errorf("minio bucket name is not configured")
	}
	uploadInfo, err := s.minioClient.PutObject(ctx, s.bucketName, objectName, reader, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("failed to upload artifact '%s': %w", objectName, err)
	}
	s.logger.Info("Stored artifact", slog.String("bucket", uploadInfo.Bucket), slog.String("key", uploadInfo.Key), slog.Int64("size", uploadInfo.Size))
	return nil
}

// UpdateJobStatus updates the status and optionally the started_at time for a job.
func (s *Store) UpdateJobStatus(ctx context.Context, jobID string, status string, details map[string]interface{}) error {
	if jobID == "" {
		return fmt.Errorf("cannot update status for empty JobID")
	}

	var startedAtArg sql.NullTime
	if status == models.StatusRunning {
		startedAtArg = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}

	var detailsJSON []byte
	var err error
	if details != nil {
		detailsJSON, err = json.Marshal(details)
		if err != nil {
			s.logger.Error("Failed to marshal details for UpdateJobStatus", slog.String("job_id", jobID), slog.String("error", err.Error()))
			return fmt.Errorf("failed to marshal details for job %s: %w", jobID, err)
		}
	} else {
		// Pass NULL to SQL if details map is nil, so COALESCE or CASE WHEN can preserve existing DB value.
		detailsJSON = nil // pgx will interpret nil []byte as NULL for JSONB if the query handles it.
	}

	cmdTag, err := s.db.Exec(ctx, updateStatusAndStartSQL, jobID, status, startedAtArg, detailsJSON)
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

// UpdateJobProgress updates the job's status, appends to logs, and merges metadata.
func (s *Store) UpdateJobProgress(ctx context.Context, jobID string, progress *models.JobProgressUpdate) error {
	if jobID == "" {
		return errors.New("job ID cannot be empty for progress update")
	}
	if progress == nil {
		return errors.New("progress update data cannot be nil")
	}

	var statusArg sql.NullString
	if progress.Status != nil {
		statusArg = sql.NullString{String: *progress.Status, Valid: true}
	}

	var logsChunkArg sql.NullString
	if progress.LogsChunk != nil {
		logsChunkArg = sql.NullString{String: *progress.LogsChunk, Valid: true}
	}

	var messagesChunkArg []string // pgx handles []string for TEXT[] directly, including nil
	if progress.MessagesChunk != nil && len(progress.MessagesChunk) > 0 {
		messagesChunkArg = progress.MessagesChunk
	}

	var passrateArg sql.NullString
	if progress.Passrate != nil {
		passrateArg = sql.NullString{String: *progress.Passrate, Valid: true}
	}

	var progressArg sql.NullString
	if progress.Progress != nil {
		progressArg = sql.NullString{String: *progress.Progress, Valid: true}
	}

	cmdTag, err := s.db.Exec(ctx, updateJobProgressSQL, jobID, statusArg, logsChunkArg, messagesChunkArg, passrateArg, progressArg)
	if err != nil {
		return fmt.Errorf("failed to execute update job progress query for job %s: %w", jobID, err)
	}

	if cmdTag.RowsAffected() == 0 {
		s.logger.Warn("Attempted to update progress for non-existent job", slog.String("job_id", jobID))
		return fmt.Errorf("job with ID %s not found for progress update", jobID) // Or return nil if "not found" is acceptable
	}

	s.logger.Info("Updated job progress in storage", slog.String("job_id", jobID))
	return nil
}

// CountJobsByStatus counts the number of jobs for a given project and status.
func (s *Store) CountJobsByStatus(ctx context.Context, project string, status string) (int, error) {
	var count int
	err := s.db.QueryRow(ctx, countJobsByStatusSQL, project, status).Scan(&count)
	if err != nil {
		// pgx.ErrNoRows is not expected for COUNT(*), it should return 0 rows with 0 value if no match.
		// Any error here is likely a connection issue or syntax error.
		s.logger.Error("Failed to count jobs by status", slog.String("project", project), slog.String("status", status), slog.String("error", err.Error()))
		return 0, fmt.Errorf("failed to count jobs for project %s with status %s: %w", project, status, err)
	}
	return count, nil
}

// GetProjectQueueOverview retrieves pending job count, running count, and highest priority for a project.
func (s *Store) GetProjectQueueOverview(ctx context.Context, project string) (*models.ProjectQueueOverview, error) {
	overview := &models.ProjectQueueOverview{
		Project: project,
	}
	var dbPriority sql.NullInt32 // Use sql.NullInt32 for MIN(priority) which can be NULL

	err := s.db.QueryRow(ctx, getProjectQueueOverviewSQL, project, models.StatusPending, models.StatusRunning).Scan(
		&overview.PendingJobs,
		&dbPriority,
		&overview.RunningSuites,
	)

	if err != nil {
		// pgx.ErrNoRows is not expected here as the query uses aggregate functions and should always return a row.
		s.logger.Error("Failed to get project queue overview", slog.String("project", project), slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to get queue overview for project %s: %w", project, err)
	}

	if dbPriority.Valid {
		highestPrio := uint8(dbPriority.Int32)
		overview.HighestPriority = &highestPrio
	}

	return overview, nil
}

func (s *Store) GeneratePresignedURL(ctx context.Context, objectName string) (string, error) {
	// Set the expiry time for the presigned URL. Let's use 15 minutes as an example.
	expiry := 15 * time.Minute

	// Generate the presigned URL. Because the client is now configured with the
	// public endpoint (and a custom transport), the generated URL will be
	// correct from the start. No rewriting is needed.
	presignedURL, err := s.minioClient.PresignedGetObject(ctx, s.bucketName, objectName, expiry, nil)
	if err != nil {
		s.logger.Error("Failed to generate presigned URL for object", "object", objectName, "error", err)
		return "", err
	}
	return presignedURL.String(), nil
}
