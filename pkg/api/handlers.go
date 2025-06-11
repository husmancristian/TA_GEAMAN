package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"

	// Removed sync import as it's no longer needed here
	"time"

	httperrors "github.com/husmancristian/TA_GEAMAN/errors" // Error helpers
	"github.com/husmancristian/TA_GEAMAN/pkg/config"        // Import config to get project list
	"github.com/husmancristian/TA_GEAMAN/pkg/models"        // Adjust import path
	"github.com/husmancristian/TA_GEAMAN/pkg/queue"
	"github.com/husmancristian/TA_GEAMAN/pkg/storage"

	"github.com/go-chi/chi/v5"
)

const (
	maxUploadMemory     = 32 << 20 // 32 MB
	resultJsonFieldName = "result"
	defaultPriority     = 5
)

type API struct {
	QueueManager queue.Manager
	ResultStore  storage.ResultStore
	Logger       *slog.Logger
	Config       *config.Config
}

func NewAPI(qm queue.Manager, rs storage.ResultStore, logger *slog.Logger, cfg *config.Config) *API {
	return &API{QueueManager: qm, ResultStore: rs, Logger: logger, Config: cfg}
}

// HandleEnqueueTest now also saves initial PENDING state to storage.
func (a *API) HandleEnqueueTest(w http.ResponseWriter, r *http.Request) {
	logger := a.Logger.With(slog.String("handler", "HandleEnqueueTest"))
	var req models.TestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperrors.BadRequest(w, logger, err, "Invalid JSON request body")
		return
	}
	defer r.Body.Close()
	if req.Project == "" {
		httperrors.BadRequest(w, logger, nil, "Missing required field: project")
		return
	}

	// Validate if the project exists in the configuration
	projectIsValid := false
	for _, p := range a.Config.Projects {
		if p == req.Project {
			projectIsValid = true
			break
		}
	}
	if !projectIsValid {
		httperrors.BadRequest(w, logger, nil, fmt.Sprintf("Project '%s' is not a configured project", req.Project))
		return
	}
	priority := req.Priority
	if priority == 0 {
		priority = defaultPriority
	}

	// 1. Enqueue the job using the interface method directly
	jobID, err := a.QueueManager.EnqueueJob(req.Project, req.Details, priority)
	if err != nil {
		logger.Error("Failed to enqueue job", slog.String("project", req.Project), slog.String("error", err.Error()))
		httperrors.InternalServerError(w, logger, err, "Failed to enqueue job")
		return
	}

	// 2. Create initial PENDING record in storage
	pendingJob := &models.TestJob{
		ID:         jobID,
		Project:    req.Project,
		Details:    req.Details,
		Priority:   priority,
		EnqueuedAt: time.Now().UTC(), // Record enqueue time
		Status:     models.StatusPending,
	}
	// Use the interface method CreatePendingJob
	if err := a.ResultStore.CreatePendingJob(r.Context(), pendingJob); err != nil {
		// Log error, but don't fail the request as job is already queued
		logger.Error("Failed to save initial pending job state to storage", slog.String("job_id", jobID), slog.String("error", err.Error()))
		// Maybe implement compensating action to remove from queue? Complex.
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// HandleGetNextTest updates status to RUNNING in storage *before* ACK.
func (a *API) HandleGetNextTest(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "project")
	logger := a.Logger.With(slog.String("handler", "HandleGetNextTest"), slog.String("project", project))
	if project == "" {
		httperrors.BadRequest(w, logger, nil, "Missing project name")
		return
	}

	// 1. Get job from queue
	jobFromQueue, ackNacker, err := a.QueueManager.GetNextJob(project)
	if err != nil {
		// This error is from queue manager itself (e.g., connection issue)
		httperrors.InternalServerError(w, logger, err, "Failed to retrieve job from queue")
		return
	}
	if jobFromQueue == nil || ackNacker == nil { // No job available in queue
		w.WriteHeader(http.StatusNoContent)
		return
	}
	logger = logger.With(slog.String("job_id", jobFromQueue.ID)) // Add job_id to logger context

	// 2. Check job's current status in the database
	dbJobState, dbErr := a.ResultStore.GetResult(r.Context(), jobFromQueue.ID)

	if dbErr != nil {
		// This implies a storage access error, not "not found" as GetResult handles that.
		logger.Error("Failed to get job state from DB", slog.String("error", dbErr.Error()))
		if nackErr := ackNacker.Nack(true); nackErr != nil { // Requeue=true, might be transient DB issue
			logger.Error("Failed to Nack message after DB get state failure", slog.String("nack_error", nackErr.Error()))
		}
		httperrors.InternalServerError(w, logger, dbErr, "Failed to verify job state in storage")
		return
	}

	if dbJobState == nil {
		// Job was in queue but not in DB. This indicates an issue with CreatePendingJob or data inconsistency.
		logger.Error("Job from queue not found in DB. NACKing without requeue.", slog.String("job_id", jobFromQueue.ID))
		if nackErr := ackNacker.Nack(false); nackErr != nil { // Requeue=false, this job is problematic
			logger.Error("Failed to Nack message for job not found in DB", slog.String("nack_error", nackErr.Error()))
		}
		httperrors.InternalServerError(w, logger, fmt.Errorf("job %s from queue not found in DB", jobFromQueue.ID), "Job state inconsistency")
		return
	}

	// Job found in DB, check its status
	currentDBStatus := dbJobState.Status
	if isTerminalStatus(currentDBStatus) {
		logger.Info("Job from queue already in terminal state in DB, discarding.", slog.String("db_status", currentDBStatus))
		if ackErr := ackNacker.Ack(); ackErr != nil {
			logger.Error("Failed to ACK already terminal job", slog.String("ack_error", ackErr.Error()))
		}
		w.WriteHeader(http.StatusNoContent) // Runner should try for another job
		return
	}

	if currentDBStatus == models.StatusRunning || currentDBStatus == models.StatusAbortRequested {
		logger.Warn("Job from queue already in RUNNING or ABORT_REQUESTED state in DB. NACKing.", slog.String("db_status", currentDBStatus))
		if nackErr := ackNacker.Nack(true); nackErr != nil { // Requeue=true, let queue/another worker sort it out
			logger.Error("Failed to Nack job already running/abort_requested", slog.String("nack_error", nackErr.Error()))
		}
		w.WriteHeader(http.StatusNoContent) // Runner should try for another job (simplest for runner)
		return
	}

	// If status is PENDING (or any other valid non-terminal, non-running state), proceed to update to RUNNING.
	if currentDBStatus != models.StatusPending {
		logger.Warn("Job from queue found in DB with unexpected non-terminal/non-running status. Proceeding to set RUNNING.",
			slog.String("db_status", currentDBStatus))
		// This case implies the DB state was something other than PENDING, RUNNING, ABORT_REQUESTED, or terminal.
		// E.g. if new states are added and not handled above. For now, we'll try to transition to RUNNING.
	}

	// 3. Update status to RUNNING in DB
	// The UpdateJobStatus method also sets started_at when status is RUNNING.
	if updateErr := a.ResultStore.UpdateJobStatus(r.Context(), jobFromQueue.ID, models.StatusRunning); updateErr != nil {
		logger.Error("Failed to update job status to RUNNING in DB", slog.String("job_id", jobFromQueue.ID), slog.String("error", updateErr.Error()))
		// If UpdateJobStatus fails (e.g., DB error, or if it were to return ErrNotFound because job disappeared),
		// Nack the message.
		if nackErr := ackNacker.Nack(true); nackErr != nil { // Requeue = true
			logger.Error("Failed to Nack message after DB update to RUNNING failure", slog.String("nack_error", nackErr.Error()))
		}
		httperrors.InternalServerError(w, logger, updateErr, "Failed to update job status in storage")
		return
	}

	// Prepare job to send to runner
	jobToRunner := jobFromQueue // This is *models.TestJob
	jobToRunner.Status = models.StatusRunning
	jobToRunner.StartedAt = time.Now().UTC() // Set for the response; DB also has its own timestamp.

	// Send response to runner
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(jobToRunner); err != nil {
		logger.Error("Failed to encode job response, Nacking message", slog.String("job_id", jobToRunner.ID), slog.String("error", err.Error()))
		if nackErr := ackNacker.Nack(true); nackErr != nil {
			logger.Error("Failed to Nack message after response encoding error", slog.String("nack_error", nackErr.Error()))
		}
		return
	}

	// ACK message only after successful response and DB update
	if ackErr := ackNacker.Ack(); ackErr != nil {
		logger.Error("Failed to ACK message after successful response", slog.String("error", ackErr.Error()))
	} else {
		logger.Info("Successfully ACKed message")
	}
}

// HandleSubmitResult remains largely the same, updates final status via SaveResult (UPSERT)
func (a *API) HandleSubmitResult(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleSubmitResult"), slog.String("job_id", jobID))
	if jobID == "" {
		httperrors.BadRequest(w, logger, nil, "Missing job ID")
		return
	}

	err := r.ParseMultipartForm(maxUploadMemory)
	if err != nil {
		httperrors.BadRequest(w, logger, err, "Failed to parse multipart form")
		return
	}
	defer r.Body.Close()

	resultJSON := r.FormValue(resultJsonFieldName)
	if resultJSON == "" {
		httperrors.BadRequest(w, logger, nil, "Missing result field")
		return
	}

	var result models.TestResult
	logger.Info("ajung la result json  ", slog.String("resultJSON", resultJSON))

	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		httperrors.BadRequest(w, logger, err, "Invalid JSON in result field")
		return
	}

	result.JobID = jobID
	if result.Status == "" {
		httperrors.BadRequest(w, logger, nil, "Missing status in result JSON")
		return
	}
	if result.EndedAt.IsZero() {
		result.EndedAt = time.Now().UTC()
	}
	if result.Duration == 0 && !result.StartedAt.IsZero() {
		result.Duration = result.EndedAt.Sub(result.StartedAt).Seconds()
	}

	screenshotURLs := []string{}

	videoURLs := []string{}
	if r.MultipartForm != nil && r.MultipartForm.File != nil {
		for key, fileHeaders := range r.MultipartForm.File {
			for _, fh := range fileHeaders {
				logger.Info("Processing uploaded file", slog.String("field", key), slog.String("filename", fh.Filename))
				artifactURL, err := parseAndStoreFile(r.Context(), a.ResultStore, fh, jobID, logger)
				logger.Info("artifactURL ", slog.String("artifactURL", artifactURL))

				if err != nil {
					httperrors.InternalServerError(w, logger, err, "Failed to store artifact")
					return
				}
				if key == "screenshots" {
					screenshotURLs = append(screenshotURLs, artifactURL)
				}
				if key == "videos" {
					videoURLs = append(videoURLs, artifactURL)
				}

				// else { logger.Warn("Uploaded file in unexpected field", slog.String("field", key)) }
			}
		}
	}
	result.Screenshots = screenshotURLs
	result.Videos = videoURLs

	// SaveResult performs UPSERT, updating status and other fields
	err = a.ResultStore.SaveResult(r.Context(), &result)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to save test result")
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Result for job %s saved successfully.", jobID)
}

// HandleGetResult remains the same
func (a *API) HandleGetResult(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleGetResult"), slog.String("job_id", jobID))
	if jobID == "" {
		httperrors.BadRequest(w, logger, nil, "Missing job ID")
		return
	}

	result, err := a.ResultStore.GetResult(r.Context(), jobID)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to retrieve result")
		return
	}
	if result == nil {
		httperrors.NotFound(w, logger, nil, "Result not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		logger.Error("Failed to encode result response", slog.String("error", err.Error()))
	}
}

// HandleGetQueueStatus remains the same
func (a *API) HandleGetQueueStatus(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "project")
	logger := a.Logger.With(slog.String("handler", "HandleGetQueueStatus"), slog.String("project", project))
	if project == "" {
		httperrors.BadRequest(w, logger, nil, "Missing project name")
		return
	}

	size, err := a.QueueManager.GetQueueSize(project)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to get queue status")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{"project": project, "pending_jobs": size})
}

// HandleGetAllQueueStatuses retrieves the pending job count for all configured projects.
func (a *API) HandleGetAllQueueStatuses(w http.ResponseWriter, r *http.Request) {
	logger := a.Logger.With(slog.String("handler", "HandleGetAllQueueStatuses"))
	projects := a.Config.Projects
	if len(projects) == 0 {
		logger.Warn("No projects configured")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]interface{}{})
		return
	}

	overview := make([]map[string]interface{}, 0, len(projects))
	var firstError error

	for _, project := range projects {
		size, err := a.QueueManager.GetQueueSize(project)
		if err != nil {
			logger.Error("Failed to get queue size for overview", slog.String("project", project), slog.String("error", err.Error()))
			if firstError == nil {
				firstError = fmt.Errorf("project %s: %w", project, err)
			}
			continue
		}
		overview = append(overview, map[string]interface{}{"project": project, "pending_jobs": size})
	}

	if firstError != nil {
		httperrors.InternalServerError(w, logger, firstError, "Failed to retrieve status for one or more queues")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(overview); err != nil {
		logger.Error("Failed to encode queue overview response", slog.String("error", err.Error()))
	}
}

// HandleGetJobs now fetches ACTIVE jobs from storage
func (a *API) HandleGetJobs(w http.ResponseWriter, r *http.Request) {
	logger := a.Logger.With(slog.String("handler", "HandleGetJobs"))
	// TODO: Add filtering/pagination based on query parameters

	// Use the ResultStore to fetch active jobs
	jobs, err := a.ResultStore.GetJobs(r.Context()) // Assumes GetJobs was added to interface and fetches active jobs
	if err != nil {
		logger.Error("Failed to retrieve active jobs from storage", slog.String("error", err.Error()))
		httperrors.InternalServerError(w, logger, err, "Failed to retrieve jobs")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(jobs); err != nil {
		logger.Error("Failed to encode jobs response", slog.String("error", err.Error()))
	}
}

// HandleGetProjectResults retrieves all results for a specific project.
func (a *API) HandleGetProjectResults(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "project")
	logger := a.Logger.With(slog.String("handler", "HandleGetProjectResults"), slog.String("project", project))
	if project == "" {
		httperrors.BadRequest(w, logger, nil, "Missing project name")
		return
	}

	results, err := a.ResultStore.GetResultsByProject(r.Context(), project)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, fmt.Sprintf("Failed to retrieve results for project %s", project))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(results); err != nil {
		logger.Error("Failed to encode project results response", slog.String("project", project), slog.String("error", err.Error()))
		// httperrors.InternalServerError is not called here as headers might have been written
	}
}

// --- Job Management Handlers (Remain the same - operate on DB status) ---

func (a *API) HandleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleCancelJob"), slog.String("job_id", jobID))
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusCancelled)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to request job cancellation")
		return
	}
	logger.Info("Job status marked as CANCELLED in storage")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Cancellation request processed for job %s (marked as cancelled).", jobID)
}
func (a *API) HandleSkipJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleSkipJob"), slog.String("job_id", jobID))
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusSkipped)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to skip job")
		return
	}
	logger.Info("Job status marked as SKIPPED in storage")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Job %s marked as skipped.", jobID)
}
func (a *API) HandleRerunJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleRerunJob"), slog.String("job_id", jobID))
	originalResult, err := a.ResultStore.GetResult(r.Context(), jobID)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Could not retrieve original job details")
		return
	}
	if originalResult == nil {
		httperrors.NotFound(w, logger, nil, fmt.Sprintf("Original job %s not found", jobID))
		return
	}
	project := originalResult.Project  // Use project from fetched result
	details := originalResult.Metadata // Assuming details were stored in metadata
	priority := uint8(defaultPriority) // Use default priority for rerun? Or original? // FIXME: Get original priority if needed
	newJobID, err := a.QueueManager.EnqueueJob(project, details, priority)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to enqueue rerun job")
		return
	}
	logger.Info("Re-queued job", slog.String("original_job_id", jobID), slog.String("new_job_id", newJobID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"new_job_id": newJobID})
}
func (a *API) HandleAbortJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleAbortJob"), slog.String("job_id", jobID))
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusAbortRequested)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to request job abortion")
		return
	}
	logger.Info("Job status marked as ABORT_REQUESTED in storage")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Abort request processed for job %s.", jobID)
}

// Helper remains the same
func parseAndStoreFile(ctx context.Context, store storage.ResultStore, fh *multipart.FileHeader, jobID string, logger *slog.Logger) (string, error) {
	file, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open uploaded file '%s': %w", fh.Filename, err)
	}
	defer file.Close()
	objectName := fmt.Sprintf("%s/artifacts/%s", jobID, fh.Filename)
	contentType := fh.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	artifactURL, err := store.StoreArtifact(ctx, objectName, file, fh.Size, contentType)
	if err != nil {
		return "", fmt.Errorf("failed to store artifact '%s': %w", objectName, err)
	}
	return artifactURL, nil
}

// isTerminalStatus checks if a job status is one that means the job processing is complete or definitively stopped.
func isTerminalStatus(status string) bool {
	switch status {
	case models.StatusPassed,
		models.StatusFailed,
		models.StatusError,
		models.StatusSkipped,
		models.StatusCancelled,
		models.StatusAborted,
		models.StatusSystemError:
		return true
	default:
		return false
	}
}
