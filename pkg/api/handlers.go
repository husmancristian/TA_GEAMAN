package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sync" // Added for mutex
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

// API struct now includes a cache for the dynamic project list.
type API struct {
	QueueManager queue.Manager
	ResultStore  storage.ResultStore // Will be used to fetch projects from DB
	Logger       *slog.Logger
	AppConfig    *config.Config // Renamed from Config to avoid confusion

	// For dynamic project list management
	projectsCache      []string
	projectsCacheMutex sync.RWMutex
	cacheRefreshTicker *time.Ticker // For periodic refresh
	cacheDoneChan      chan bool    // To stop the ticker goroutine
}

// NewAPI initializes the API, populates the project cache, and starts periodic refresh.
func NewAPI(qm queue.Manager, rs storage.ResultStore, logger *slog.Logger, cfg *config.Config) *API {
	api := &API{
		QueueManager:       qm,
		ResultStore:        rs,
		Logger:             logger,
		AppConfig:          cfg,
		cacheRefreshTicker: time.NewTicker(5 * time.Minute), // Example: refresh every 5 mins
		cacheDoneChan:      make(chan bool),
	}

	// Initial cache population
	if err := api.refreshProjectsCache(context.Background()); err != nil {
		logger.Error("Failed to perform initial population of projects cache", slog.String("error", err.Error()))
		// Consider how to handle this: panic, or run with potentially empty/stale cache.
		// For now, we log and continue.
	}

	// Start periodic cache refresh in a separate goroutine
	go api.runPeriodicCacheRefresh()

	return api
}

// ShutdownCacheRefresher should be called during graceful application shutdown.
func (a *API) ShutdownCacheRefresher() {
	if a.cacheRefreshTicker != nil {
		a.cacheRefreshTicker.Stop()
	}
	if a.cacheDoneChan != nil {
		// Check if channel is already closed to prevent panic
		select {
		case <-a.cacheDoneChan:
			// Already closed or received signal
		default:
			close(a.cacheDoneChan)
		}
	}
	a.Logger.Info("Project cache refresher shut down.")
}

func (a *API) refreshProjectsCache(ctx context.Context) error {
	a.Logger.Debug("Attempting to refresh projects cache from storage")
	projects, err := a.ResultStore.GetProjects(ctx) // Uses new storage method
	if err != nil {
		a.Logger.Error("Failed to get projects from storage for cache refresh", slog.String("error", err.Error()))
		return fmt.Errorf("ResultStore.GetProjects failed: %w", err)
	}

	a.projectsCacheMutex.Lock()
	a.projectsCache = projects
	a.projectsCacheMutex.Unlock()
	a.Logger.Info("Projects cache refreshed", slog.Int("count", len(projects)), slog.Any("projects", projects))
	return nil
}

func (a *API) runPeriodicCacheRefresh() {
	defer func() {
		if r := recover(); r != nil {
			a.Logger.Error("Panic recovered in runPeriodicCacheRefresh", slog.Any("panic", r))
		}
	}()
	for {
		select {
		case <-a.cacheDoneChan:
			a.Logger.Info("Stopping periodic projects cache refresh.")
			return
		case <-a.cacheRefreshTicker.C:
			a.Logger.Debug("Periodic projects cache refresh triggered.")
			if err := a.refreshProjectsCache(context.Background()); err != nil {
				// Error is logged within refreshProjectsCache
			}
		}
	}
}

// isProjectValid checks against the cached projects list.
func (a *API) isProjectValid(projectName string) bool {
	a.projectsCacheMutex.RLock()
	defer a.projectsCacheMutex.RUnlock()
	for _, p := range a.projectsCache {
		if p == projectName {
			return true
		}
	}
	return false
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

	// Validate if the project exists using the cached list
	if !a.isProjectValid(req.Project) {
		httperrors.BadRequest(w, logger, nil, fmt.Sprintf("Project '%s' is not a configured project", req.Project))
		return
	}

	// Log the priority received from the request body AFTER decoding
	logger.Info("Priority from decoded request body", slog.Uint64("req.Priority_after_decode", uint64(req.Priority)))

	priority := req.Priority // User-defined priority. If 0, it's intended as highest.
	// The defaultPriority constant might be used if req.Priority could be nil (e.g. *uint8),
	// indicating "not set". With uint8, omitted is 0, which now means highest.
	// Log the priority value that will be passed to EnqueueJob
	logger.Info("Priority value being passed to EnqueueJob", slog.Uint64("priority_to_enqueue", uint64(priority)))

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
	if errDb := a.ResultStore.CreatePendingJob(r.Context(), pendingJob); errDb != nil {
		// Log error, but don't fail the request as job is already queued
		logger.Error("Failed to save initial pending job state to storage", slog.String("job_id", jobID), slog.String("error", errDb.Error()))
		// For now, return an error to the client as the operation was not fully successful.
		httperrors.InternalServerError(w, logger, errDb, fmt.Sprintf("Failed to save initial state for job %s", jobID))
		return
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

	if currentDBStatus == models.StatusRunning {
		logger.Warn("Job from queue already in RUNNING state in DB. NACKing (requeue).", slog.String("db_status", currentDBStatus))
		if nackErr := ackNacker.Nack(true); nackErr != nil { // Requeue=true
			logger.Error("Failed to Nack job already running", slog.String("nack_error", nackErr.Error()))
		}
		w.WriteHeader(http.StatusNoContent) // Runner should try for another job (simplest for runner)
		return
	} else if currentDBStatus == models.StatusAbortRequested {
		logger.Info("Job from queue already in ABORT_REQUESTED state in DB. ACK'ing and discarding.", slog.String("db_status", currentDBStatus))
		if ackErr := ackNacker.Ack(); ackErr != nil {
			logger.Error("Failed to ACK already ABORT_REQUESTED job", slog.String("ack_error", ackErr.Error()))
		}
		w.WriteHeader(http.StatusNoContent) // Runner should try for another job
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
	// Prepare details to update, including runner information.
	updatedDetails := dbJobState.Details // Get existing details from the fetched TestResult
	if updatedDetails == nil {
		updatedDetails = make(map[string]interface{})
	}

	// Add runner information.
	runnerID := r.Header.Get("X-Runner-ID") // Attempt to get runner ID from a custom header
	if runnerID == "" {
		runnerID = "unknown" // Default if header is not present
	}
	updatedDetails["runner_id"] = runnerID

	// The UpdateJobStatus method also sets started_at when status is RUNNING.
	if updateErr := a.ResultStore.UpdateJobStatus(r.Context(), jobFromQueue.ID, models.StatusRunning, updatedDetails); updateErr != nil {

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
	jobToRunner.Details = updatedDetails     // Ensure the details sent back include the runner_id
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
				if key == "log_file" { // Add this condition
					result.Logs = artifactURL // Assign the log file URL to result.Logs
					logger.Info("Assigned log_file URL to result.Logs", slog.String("url", result.Logs))
				}

				// else { logger.Warn("Uploaded file in unexpected field", slog.String("field", key)) }
			}
		}
	}
	result.Screenshots = screenshotURLs
	result.Videos = videoURLs

	logger.Info("Calling SaveResult with result.Logs", slog.String("job_id", result.JobID), slog.String("logs_value", result.Logs))
	// SaveResult performs UPSERT, updating status and other fields
	err = a.ResultStore.SaveResult(r.Context(), &result)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to save test result")
		return
	}

	// Return the updated result object, which now includes the log file URL
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		logger.Error("Failed to encode result response after saving", slog.String("error", err.Error()))
	}
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

	a.projectsCacheMutex.RLock()
	projects := make([]string, len(a.projectsCache))
	copy(projects, a.projectsCache)
	a.projectsCacheMutex.RUnlock()

	if len(projects) == 0 {
		logger.Warn("No projects configured")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]interface{}{})
		return
	}

	overviewResponse := make([]models.ProjectQueueOverview, 0, len(projects))
	var firstError error

	for _, project := range projects {
		projectOverview, err := a.ResultStore.GetProjectQueueOverview(r.Context(), project)
		if err != nil {
			logger.Error("Failed to get project queue overview", slog.String("project", project), slog.String("error", err.Error()))
			if firstError == nil {
				firstError = fmt.Errorf("failed to get overview for project %s: %w", project, err)
			}
			// Continue to try and fetch for other projects, but report the first error.
			continue // Skip appending this project if there was an error
		}
		if projectOverview != nil {
			overviewResponse = append(overviewResponse, *projectOverview)
		}
	}

	if firstError != nil {
		httperrors.InternalServerError(w, logger, firstError, "Failed to retrieve status for one or more queues")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(overviewResponse); err != nil {
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
	project := chi.URLParam(r, "projectName")
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
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusCancelled, nil)
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
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusSkipped, nil)
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
	details := originalResult.Details  // Original run details
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
	err := a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusAbortRequested, nil)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to request job abortion")
		return
	}
	logger.Info("Job status marked as ABORT_REQUESTED in storage")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Abort request processed for job %s.", jobID)
}

// HandlePrioritizeJob re-queues an existing PENDING job with the highest priority.
func (a *API) HandlePrioritizeJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandlePrioritizeJob"), slog.String("original_job_id", jobID))

	originalResult, err := a.ResultStore.GetResult(r.Context(), jobID)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Could not retrieve original job details to prioritize")
		return
	}
	if originalResult == nil {
		httperrors.NotFound(w, logger, nil, fmt.Sprintf("Original job %s not found to prioritize", jobID))
		return
	}

	if originalResult.Status != models.StatusPending {
		httperrors.BadRequest(w, logger, nil, fmt.Sprintf("Job %s is not in PENDING state, cannot prioritize. Current status: %s", jobID, originalResult.Status))
		return
	}

	// 1. Mark the original job as cancelled (or superseded)
	// Using StatusCancelled for simplicity, as HandleGetNextTest already handles it.
	err = a.ResultStore.UpdateJobStatus(r.Context(), jobID, models.StatusCancelled, map[string]interface{}{"reason": fmt.Sprintf("Superseded by priority request for new job")})
	if err != nil {
		httperrors.InternalServerError(w, logger, err, fmt.Sprintf("Failed to update original job %s status before prioritizing", jobID))
		return
	}
	logger.Info("Marked original job as CANCELLED before re-queueing with high priority", slog.String("original_job_id", jobID))

	// 2. Enqueue a new job with original details and highest priority
	highestPriority := uint8(0) // Assuming 0 is highest
	newJobID, err := a.QueueManager.EnqueueJob(originalResult.Project, originalResult.Details, highestPriority)
	if err != nil {
		// Potentially try to revert the original job's status if this fails? Complex.
		httperrors.InternalServerError(w, logger, err, "Failed to enqueue new high-priority job")
		return
	}

	// 3. Create initial PENDING record for the new high-priority job
	// This is similar to HandleEnqueueTest
	pendingJob := &models.TestJob{ID: newJobID, Project: originalResult.Project, Details: originalResult.Details, Priority: highestPriority, EnqueuedAt: time.Now().UTC(), Status: models.StatusPending}
	if errDb := a.ResultStore.CreatePendingJob(r.Context(), pendingJob); errDb != nil {
		logger.Error("Failed to save initial pending state for new high-priority job to storage", slog.String("new_job_id", newJobID), slog.String("original_job_id", jobID), slog.String("error", errDb.Error()))
		// Job is in queue, but DB record failed. Critical to log.
	}

	logger.Info("Successfully re-queued job with high priority", slog.String("original_job_id", jobID), slog.String("new_job_id", newJobID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Job prioritized successfully", "original_job_id": jobID, "new_job_id": newJobID})
}

// HandleGetJobStatusCheck returns the current status of a specific job.
// This is useful for runners to poll if an abort has been requested.
func (a *API) HandleGetJobStatusCheck(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleGetJobStatusCheck"), slog.String("job_id", jobID))

	if jobID == "" {
		httperrors.BadRequest(w, logger, nil, "Missing job ID in URL path")
		return
	}

	result, err := a.ResultStore.GetResult(r.Context(), jobID)
	if err != nil {
		httperrors.InternalServerError(w, logger, err, "Failed to retrieve job status")
		return
	}
	if result == nil {
		httperrors.NotFound(w, logger, nil, fmt.Sprintf("Job %s not found", jobID))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"job_id": result.JobID, "status": result.Status}); err != nil {
		logger.Error("Failed to encode job status response", slog.String("error", err.Error()))
	}
}

// HandleUpdateJobProgress handles real-time updates to a job's progress.
func (a *API) HandleUpdateJobProgress(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	logger := a.Logger.With(slog.String("handler", "HandleUpdateJobProgress"), slog.String("job_id", jobID))

	if jobID == "" {
		httperrors.BadRequest(w, logger, nil, "Missing job ID in URL path")
		return
	}

	var progressUpdate models.JobProgressUpdate
	if err := json.NewDecoder(r.Body).Decode(&progressUpdate); err != nil {
		httperrors.BadRequest(w, logger, err, "Invalid JSON request body for progress update")
		return
	}
	defer r.Body.Close()

	// Call the storage method to update progress
	if err := a.ResultStore.UpdateJobProgress(r.Context(), jobID, &progressUpdate); err != nil {
		// Check if the error is because the job was not found, or a general DB error
		// The current store implementation returns an error if RowsAffected is 0.
		if err.Error() == fmt.Sprintf("job with ID %s not found for progress update", jobID) { // Basic check
			httperrors.NotFound(w, logger, err, fmt.Sprintf("Job %s not found for progress update", jobID))
		} else {
			httperrors.InternalServerError(w, logger, err, "Failed to update job progress")
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Job progress updated successfully", "job_id": jobID})
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

// --- Project Management Handlers (Modified to use ResultStore and cache) ---

type projectManagementRequest struct {
	Name string `json:"name"`
}

// HandleAddProject adds a new project to the database and refreshes the cache.
func (a *API) HandleAddProject(w http.ResponseWriter, r *http.Request) {
	logger := a.Logger.With(slog.String("handler", "HandleAddProject"))
	var req projectManagementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperrors.BadRequest(w, logger, err, "Invalid JSON request body")
		return
	}
	defer r.Body.Close()

	if req.Name == "" {
		httperrors.BadRequest(w, logger, nil, "Missing required field: name")
		return
	}

	// Check if project already exists (optional, DB constraint should handle this too)
	if a.isProjectValid(req.Name) {
		httperrors.InternalServerError(w, logger, nil, fmt.Sprintf("Project '%s' already exists or is cached", req.Name))
		return
	}

	// Add to database
	if err := a.ResultStore.AddProject(r.Context(), req.Name); err != nil {
		// Handle specific DB errors, e.g., unique constraint violation as conflict
		logger.Error("Failed to add project to database", slog.String("project_name", req.Name), slog.String("error", err.Error()))
		httperrors.InternalServerError(w, logger, err, "Failed to add project")
		return
	}

	// Refresh cache
	if err := a.refreshProjectsCache(r.Context()); err != nil {
		logger.Error("Failed to refresh project cache after adding project", slog.String("project_name", req.Name), slog.String("error", err.Error()))
		// Project was added to DB, but cache refresh failed. Log and inform user.
		httperrors.InternalServerError(w, logger, err, "Project added, but cache refresh failed. Changes may not be immediately visible.")
		return
	}

	logger.Info("Added new project and refreshed cache", slog.String("project_name", req.Name))
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"message": "Project added successfully", "project_name": req.Name})
}

// HandleDeleteProject removes a project from the database and refreshes the cache.
func (a *API) HandleDeleteProject(w http.ResponseWriter, r *http.Request) {
	projectName := chi.URLParam(r, "projectName") // Ensure this matches the route param in routes.go
	logger := a.Logger.With(slog.String("handler", "HandleDeleteProject"), slog.String("project_name", projectName))

	if projectName == "" {
		httperrors.BadRequest(w, logger, nil, "Missing project name in URL path")
		return
	}

	// Check for delete protection key if it's configured
	if a.AppConfig.DeleteProtectionKey != "" {
		headerKey := r.Header.Get("X-Delete-Key")
		if headerKey == "" {
			httperrors.BadRequest(w, logger, nil, "sike")
			return
		}
		if headerKey != a.AppConfig.DeleteProtectionKey {
			httperrors.BadRequest(w, logger, nil, "no")
			return
		}
	}

	// Delete from database
	if err := a.ResultStore.DeleteProject(r.Context(), projectName); err != nil {
		// Handle specific DB errors, e.g., if project not found
		logger.Error("Failed to delete project from database", slog.String("project_name", projectName), slog.String("error", err.Error()))
		httperrors.InternalServerError(w, logger, err, "Failed to delete project")
		return
	}

	// Refresh cache
	if err := a.refreshProjectsCache(r.Context()); err != nil {
		logger.Error("Failed to refresh project cache after deleting project", slog.String("project_name", projectName), slog.String("error", err.Error()))
		httperrors.InternalServerError(w, logger, err, "Project deleted, but cache refresh failed. Changes may not be immediately visible.")
		return
	}

	logger.Info("Deleted project and refreshed cache", slog.String("project_name", projectName))
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Project deleted successfully", "project_name": projectName})
}
