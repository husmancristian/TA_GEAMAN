package models

import "time"

// TestRequest is the data received to enqueue a new test run
type TestRequest struct {
	Project string `json:"project"` // Identifies the target project queue (Required)
	// Details contains flexible data about the test (e.g., test suite name, environment, parameters)
	Details map[string]interface{} `json:"details"`
	// Priority for the job in the queue (optional, e.g., 0-10 for RabbitMQ)
	Priority uint8 `json:"priority,omitempty"`
}

// TestJob represents a unit of work dequeued by a runner or stored
type TestJob struct {
	ID         string                 `json:"id"`          // Unique identifier for this job run
	Project    string                 `json:"project"`     // Project this job belongs to
	Details    map[string]interface{} `json:"details"`     // Details provided in the request
	Priority   uint8                  `json:"priority"`    // Priority the job was enqueued with
	EnqueuedAt time.Time              `json:"enqueued_at"` // Time the job was added to the queue
	// Status can be updated in storage, might differ from queue status if tracked separately
	Status         string    `json:"status"`
	AssignedRunner string    `json:"assigned_runner,omitempty"` // ID of the runner processing it
	StartedAt      time.Time `json:"started_at,omitempty"`      // Set when runner starts
	EndedAt        time.Time `json:"ended_at,omitempty"`        // Set when runner finishes (or server marks completion)
}

// TestResult is the data submitted by the runner after execution
type TestResult struct {
	JobID       string                 `json:"job_id"`                // Usually taken from URL path, not body
	Status      string                 `json:"status"`                // Final status: PASSED, FAILED, ERROR, SKIPPED
	Project     string                 `json:"project"`               // Project title
	Logs        string                 `json:"logs,omitempty"`        // Captured logs (can be large)
	Messages    []string               `json:"messages,omitempty"`    // Status messages during the run
	Duration    float64                `json:"duration_seconds"`      // How long the test took in seconds
	StartedAt   time.Time              `json:"started_at"`            // When the runner started the job
	EndedAt     time.Time              `json:"ended_at"`              // When the runner finished the job
	Screenshots []string               `json:"screenshots,omitempty"` // URLs/paths to stored screenshots
	Videos      []string               `json:"videos,omitempty"`      // URLs/paths to stored videos
	Metadata    map[string]interface{} `json:"metadata,omitempty"`    // Any other specific result data
}

// Constants for Job Status
const (
	StatusPending        = "PENDING"
	StatusRunning        = "RUNNING"
	StatusPassed         = "PASSED"
	StatusFailed         = "FAILED"
	StatusError          = "ERROR" // Error during test execution
	StatusSkipped        = "SKIPPED"
	StatusCancelled      = "CANCELLED"       // Cancelled via API before running
	StatusAbortRequested = "ABORT_REQUESTED" // Abort requested via API while running
	StatusAborted        = "ABORTED"         // Runner confirmed abort / timed out after request
	StatusSystemError    = "SYSTEM_ERROR"    // Error within the server/queue/storage
)

// JobMessage is the structure published to RabbitMQ
type JobMessage struct {
	ID         string                 `json:"id"`
	Project    string                 `json:"project"`
	Details    map[string]interface{} `json:"details"`
	Priority   uint8                  `json:"priority"`
	EnqueuedAt time.Time              `json:"enqueued_at"`
}
