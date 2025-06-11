package queue

import "github.com/husmancristian/TA_GEAMAN/pkg/models" // Adjust import path if needed

type AckNacker interface {
	Ack() error              // Acknowledge successful processing.
	Nack(requeue bool) error // Reject processing. requeue=true puts back in queue.
}

// Manager defines the interface for managing project-specific test queues.
type Manager interface {
	// EnqueueJob adds a new test job to the specified project's queue.
	// It requires project name, job details, and a priority level.
	// Returns the unique ID assigned to the job or an error.
	EnqueueJob(project string, details map[string]interface{}, priority uint8) (string, error)

	// GetNextJob retrieves the next available test job from the specified project's queue.
	// It returns the job details, an AckNacker interface to handle the message outcome,
	// or nil if the queue is empty, along with any error during retrieval.
	GetNextJob(project string) (*models.TestJob, AckNacker, error)

	// GetQueueSize returns the current number of pending jobs in a specific project's queue.
	GetQueueSize(project string) (int, error)

	// Close releases any resources held by the queue manager (e.g., connections).
	Close() error
}
