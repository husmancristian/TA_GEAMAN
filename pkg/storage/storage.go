package storage

import (
	"context"
	"io"

	"github.com/husmancristian/TA_GEAMAN/pkg/models" // Adjust import path
)

// ResultStore defines the interface for storing and retrieving test results and artifacts.
type ResultStore interface {
	// CreatePendingJob saves the initial state when a job is first enqueued.
	CreatePendingJob(ctx context.Context, job *models.TestJob) error // Added method

	// SaveResult stores/updates the details of a test run, typically after completion.
	SaveResult(ctx context.Context, result *models.TestResult) error

	// GetResult retrieves the result metadata for a specific job ID.
	GetResult(ctx context.Context, jobID string) (*models.TestResult, error)

	// StoreArtifact handles the storage of a binary artifact (e.g., to MinIO/S3).
	StoreArtifact(ctx context.Context, objectName string, reader io.Reader, size int64, contentType string) error

	// GetJobs retrieves a list of ACTIVE jobs (Pending, Running, etc.).
	GetJobs(ctx context.Context) ([]models.TestJob, error)

	// GetResultsByProject retrieves all test results for a given project.
	GetResultsByProject(ctx context.Context, project string) ([]models.ProjectResultSummary, error)

	// UpdateJobStatus updates the status and potentially other fields (like started_at) for a job.
	UpdateJobStatus(ctx context.Context, jobID string, status string, details map[string]interface{}) error

	// UpdateJobProgress updates parts of a job's result in real-time, like status, logs, and metadata.
	UpdateJobProgress(ctx context.Context, jobID string, progress *models.JobProgressUpdate) error

	// CountJobsByStatus counts the number of jobs for a given project and status.
	CountJobsByStatus(ctx context.Context, project string, status string) (int, error)

	// GetProjectQueueOverview retrieves pending job count, running count, and highest priority for a project.
	GetProjectQueueOverview(ctx context.Context, project string) (*models.ProjectQueueOverview, error)

	// --- Project Management ---
	AddProject(ctx context.Context, projectName string) error
	DeleteProject(ctx context.Context, projectName string) error
	GetProjects(ctx context.Context) ([]string, error) // Gets all currently active/configured projects

	// GeneratePresignedURL generates a temporary, public URL for a private object.
	GeneratePresignedURL(ctx context.Context, objectName string) (string, error)

	// Close releases any resources held by the store (e.g., DB connections).
	Close() error
}
