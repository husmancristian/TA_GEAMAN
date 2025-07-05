package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/url"
	"path"
	"path/filepath"
	
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"runtime"
	"syscall" 	
	"github.com/joho/godotenv"
)

// TestJob represents a job received by the runner from the API
type TestJob struct {
	ID         string         `json:"id"`
	Project    string         `json:"project"`
	Details    map[string]any `json:"details"`
	Priority   uint8          `json:"priority"`
	EnqueuedAt time.Time      `json:"enqueued_at"`
	Status     string         `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	EndedAt    time.Time      `json:"ended_at"`
}

// ProjectQueueOverview matches the structure from the API
type ProjectQueueOverview struct {
	Project         string `json:"project"`
	PendingJobs     int    `json:"pending_jobs"`
	RunningSuites   int    `json:"running_suites"`
	HighestPriority *uint8 `json:"highest_priority,omitempty"` // Pointer to handle null
}

type JobStatusResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// FileAttachment represents a file to be attached to the multipart request.
type FileAttachment struct {
	FieldName string // e.g., "screenshots", "videos", "log_file"
	FilePath  string // Path to the file on disk
}

// TestResult is the data submitted by the runner after execution
type TestResult struct {
	JobID       string                 `json:"job_id"`                // Usually taken from URL path, not body
	Status      string                 `json:"status"`                // Final status: PASSED, FAILED, ERROR, SKIPPED
	Project     string                 `json:"project"`               // Project title
	Details     map[string]interface{} `json:"details"`               // Details provided in the original request
	Priority    uint8                  `json:"priority"`              // Priority the job was enqueued with
	EnqueuedAt  time.Time              `json:"enqueued_at"`           // Time the job was added to the queue
	Logs        string                 `json:"logs,omitempty"`        // Captured logs (can be large)
	Messages    []string               `json:"messages,omitempty"`    // Status messages during the run
	Duration    float64                `json:"duration_seconds"`      // How long the test took in seconds
	StartedAt   time.Time              `json:"started_at"`            // When the runner started the job
	EndedAt     time.Time              `json:"ended_at"`              // When the runner finished the job
	Screenshots []string               `json:"screenshots,omitempty"` // URLs/paths to stored screenshots
	Videos      []string               `json:"videos,omitempty"`      // URLs/paths to stored videos
	Passrate    string                 `json:"passrate,omitempty"`    // Passrate string, e.g., "85%" or "10/12"
	Progress    string                 `json:"progress,omitempty"`    // Progress string, e.g., "Step 5/10" or "75%"
	Metadata    map[string]interface{} `json:"metadata,omitempty"`    // Any other specific result data
}

var httpClient *http.Client

func main() {
	// Load .env file. Path can be adjusted if .env is not in the same directory as the executable.
	// Use "automation/.env" for clarity if the .env file is inside the automation folder relative to where you run `go build` or `go run`
	err := godotenv.Load("automation/.env")
	if err != nil {
		log.Println("No .env file found, relying on environment variables")
	}
	runnerID := os.Getenv("RUNNER_ID")
	if runnerID == "" {
		log.Fatal("RUNNER_ID not set in .env file or environment variables")
	}

	apiBaseURL := os.Getenv("API_BASE_URL") // e.g., https://localhost:8080/api/v1
	if apiBaseURL == "" {
		log.Fatal("API_BASE_URL not set")
	}

	assignedProjectsStr := os.Getenv("ASSIGNED_PROJECTS") // e.g., "projectA,projectB,projectC"
	if assignedProjectsStr == "" {
		log.Fatal("ASSIGNED_PROJECTS not set in .env file or environment variables")
	}
	assignedProjects := strings.Split(assignedProjectsStr, ",")
	assignedProjectsMap := make(map[string]bool)
	for _, p := range assignedProjects {
		trimmedProject := strings.TrimSpace(p)
		if trimmedProject != "" {
			assignedProjectsMap[trimmedProject] = true
		}
	}

	// Default polling interval in seconds
	defaultPollingIntervalSeconds := 30
	pollingIntervalSeconds := defaultPollingIntervalSeconds

	pollingRateEnv := os.Getenv("POLLING_INTERVAL_SECONDS")
	if pollingRateEnv != "" {
		parsedRate, err := strconv.Atoi(pollingRateEnv)
		if err == nil && parsedRate > 0 {
			pollingIntervalSeconds = parsedRate
		} else {
			log.Printf("Invalid POLLING_INTERVAL_SECONDS value: '%s'. Using default: %d seconds. Error: %v", pollingRateEnv, defaultPollingIntervalSeconds, err)
		}
	}

	log.Printf("Runner started. \n ID: %s \n API: %s \n PollingInterval: %d seconds \n Assigned Projects: %v", runnerID, apiBaseURL, pollingIntervalSeconds, assignedProjects)

	// --- Initialize secure HTTP client ---
	// Load the server's self-signed certificate to trust it.
	caCert, err := os.ReadFile("localhost+2.pem")
	if err != nil {
		log.Fatalf("Error reading server certificate file: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tr := &http.Transport{
		// Use the certificate pool to validate the server's certificate.
		// This is the secure alternative to InsecureSkipVerify.
		TLSClientConfig: &tls.Config{
			RootCAs: caCertPool,
		},
	}
	httpClient = &http.Client{
		Timeout:   time.Second * 30, // Set a reasonable timeout
		Transport: tr,               // Use the custom transport
	}

	// Main loop for the runner
	for {
		// 1. Get queue overview for all projects
		allOverviews, err := getQueuesOverview(apiBaseURL)
		if err != nil {
			log.Printf("Error getting queues overview: %v. Retrying after sleep.", err)
			time.Sleep(time.Duration(pollingIntervalSeconds) * time.Second)
			continue
		}

		// 2. Select the best project to work on from assigned projects
		bestProjectToPoll := selectBestProject(allOverviews, assignedProjectsMap)

		if bestProjectToPoll != "" {
			log.Printf("Selected project '%s' to poll for a job based on priority.", bestProjectToPoll)
			// 3. Call ta_geaman API to get the next job for this specific project
			job, err := getNextJob(apiBaseURL, bestProjectToPoll, runnerID)
			if err != nil {
				log.Printf("Error getting job for project %s: %v", bestProjectToPoll, err)
				// Let the main loop sleep and re-evaluate.
			} else if job != nil {
				// Refactored job processing logic into its own function for clarity.
				processJob(apiBaseURL, job)
			} else {
				log.Printf("No job available for selected project: %s", bestProjectToPoll)
			}
		} else {
			log.Println("No suitable projects with pending jobs found for this runner in the current overview.")
		}

		// Wait for a bit before polling again
		time.Sleep(time.Duration(pollingIntervalSeconds) * time.Second) // Configurable polling interval
	}
}

// processJob handles the entire lifecycle of executing a single test job.
func processJob(apiBaseURL string, job *TestJob) {
	log.Printf("Received job ID: %s for project: %s.", job.ID, job.Project)

	jobCtx, cancelJobExecution := context.WithCancel(context.Background())
	defer cancelJobExecution()

	var scriptOutput *TestResult
	var attachmentFiles []FileAttachment
	var scriptErr error
	var scriptLogs string

	scriptDoneChan := make(chan struct{})
	go func() {
		defer close(scriptDoneChan)
		log.Printf("[%s] Calling executeScript...", job.ID)
		scriptOutput, attachmentFiles, scriptLogs, scriptErr = executeScript(jobCtx, apiBaseURL, job)
		log.Printf("Script logs  %s.", scriptLogs)
		log.Printf("Script error  %s.", scriptErr)


	}()

	abortCheckTicker := time.NewTicker(5 * time.Second)
	defer abortCheckTicker.Stop()

	keepChecking := true
	for keepChecking {
		select {
		case <-scriptDoneChan:
			log.Printf("Script execution finished for job %s.", job.ID)
			keepChecking = false // Exit the loop
		case <-abortCheckTicker.C:
			currentJobState, err := getCurrentJobStatus(apiBaseURL, job.ID)
			if err != nil {
				log.Printf("Error checking job status for %s: %v. Will retry.", job.ID, err)
				continue
			}
			
			// If server wants to abort, just cancel the context and let the main logic handle it.
			if currentJobState == "ABORTED" || currentJobState == "CANCELLED" || currentJobState == "ABORT_REQUESTED" {
				log.Printf("Server status for job %s is '%s'. Cancelling local execution.", job.ID, currentJobState)
				cancelJobExecution()
				keepChecking = false // The cancellation will cause scriptDoneChan to close.
			}
		}
	}
	// Wait for the script goroutine to fully exit, even if it finished before the abort check.
	<-scriptDoneChan

	// The scriptErr and scriptOutput now reliably reflect the final state (including ABORTED).
	log.Printf("Reporting final results for job %s. Final Status determined by script: '%s'", job.ID, scriptOutput.Status)
	if err := postJobResultBackup(apiBaseURL, job.ID, scriptOutput, attachmentFiles); err != nil {
		log.Printf("Error reporting final results for job %s: %v", job.ID, err)
	} else {
		log.Printf("Successfully reported final results for job %s.", job.ID)
	}
}

func getNextJob(apiBaseURL, project, runnerID string) (*TestJob, error) {
	// Construct the URL: e.g., https://localhost:8080/api/v1/tests/VPN_Desktop/next
	url := fmt.Sprintf("%s/tests/%s/next", apiBaseURL, project)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add the X-Runner-ID header
	req.Header.Add("X-Runner-ID", runnerID)
	req.Header.Add("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil // No job available
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api request failed with status %s: %s", resp.Status, string(bodyBytes))
	}

	var job TestJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode job response: %w", err)
	}

	return &job, nil
}

func getQueuesOverview(apiBaseURL string) ([]ProjectQueueOverview, error) {
	url := fmt.Sprintf("%s/queues/overview", apiBaseURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create overview request: %w", err)
	}
	req.Header.Add("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute overview request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body) // Best effort to read body
		return nil, fmt.Errorf("overview api request failed with status %s: %s", resp.Status, string(bodyBytes))
	}

	var overviews []ProjectQueueOverview
	if err := json.NewDecoder(resp.Body).Decode(&overviews); err != nil {
		return nil, fmt.Errorf("failed to decode queues overview response: %w", err)
	}

	return overviews, nil
}

func selectBestProject(overviews []ProjectQueueOverview, assignedProjects map[string]bool) string {
	var bestProject string
	// Priority 0 is highest. We're looking for the minimum numeric value.
	minPriorityValue := uint8(10)

	for _, overview := range overviews {
		projectName := strings.TrimSpace(overview.Project)
		// Check if project is assigned to this runner and has pending jobs
		if _, isAssigned := assignedProjects[projectName]; !isAssigned || overview.PendingJobs == 0 {
			continue
		}

		currentProjectPriorityValue := uint8(10) // Default to lowest if HighestPriority is nil
		if overview.HighestPriority != nil {
			currentProjectPriorityValue = uint8(*overview.HighestPriority)
		}

		if currentProjectPriorityValue < minPriorityValue {
			minPriorityValue = currentProjectPriorityValue
			bestProject = projectName
		}
		// (e.g., project with more pending jobs, or fewer running suites).
		// For now, the first project encountered with the best priority wins.
	}
	return bestProject
}

// isTerminalStatus checks if a status string represents a final state for a job.
func isTerminalStatus(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "ABORTED", "CANCELLED", "ERROR", "ABORT_REQUESTED":
		return true
	default:
		return false
	}
}

// getLocalPathFromURL derives a local directory name from a git repository URL.
// e.g., "https://github.com/user/repo.git" -> "repo"
func getLocalPathFromURL(repoURL string) (string, error) {
	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	// Get the last part of the path
	localPath := path.Base(parsedURL.Path)
	// Remove .git suffix if it exists
	localPath = strings.TrimSuffix(localPath, ".git")
	if localPath == "" || localPath == "." || localPath == "/" {
		return "", fmt.Errorf("could not determine a valid directory name from URL")
	}
	return localPath, nil
}

func executeScript(ctx context.Context, apiBaseURL string, job *TestJob) (output *TestResult, attachments []FileAttachment, scriptLogs string, err error) {
	log.Printf("[%s]  executeScript: Starting for job ID: %s. Details: %+v.", job.ID, job.ID, job.Details)

	// --- Git Clone/Pull Logic 
	var executionDir string
	var logPathFromScriptOutput string
	if scriptSourceURL, ok := job.Details["script_source"].(string); ok && scriptSourceURL != "" {
		localRepoPath, pathErr := getLocalPathFromURL(scriptSourceURL)
		if pathErr != nil {
			log.Printf("[%s] Warning: Could not derive local path from script source URL '%s': %v. Will attempt to run with local sources.", job.ID, scriptSourceURL, pathErr)
		} else {
			if _, statErr := os.Stat(localRepoPath); os.IsNotExist(statErr) {
				log.Printf("[%s] Cloning repository from %s into %s", job.ID, scriptSourceURL, localRepoPath)
				gitCmd := exec.Command("git", "clone", scriptSourceURL, localRepoPath)
				output, gitErr := gitCmd.CombinedOutput()
				if gitErr != nil {
					log.Printf("[%s] Warning: 'git clone' failed: %v. Output: %s. Will attempt to run with local sources.", job.ID, gitErr, string(output))
				} else {
					executionDir = localRepoPath
				}
			} else {
				log.Printf("[%s] Force pulling latest changes for repository in %s", job.ID, localRepoPath)
				commands := [][]string{
					{"git", "fetch", "--all"},
					{"git", "reset", "--hard", "origin/main"},
					{"git", "clean", "-fdx"},
				}
				var pullErr error
				for _, c := range commands {
					cmd := exec.Command(c[0], c[1:]...)
					cmd.Dir = localRepoPath
					if output, err := cmd.CombinedOutput(); err != nil {
						log.Printf("[%s] Git command '%s' failed: %v. Output: %s", job.ID, strings.Join(c, " "), err, string(output))
						pullErr = err
						break
					}
				}
				if pullErr != nil {
					log.Printf("[%s] Force pull encountered errors. Will attempt to run with local sources.", job.ID)
				}
				executionDir = localRepoPath
			}
		}
	}

	result := &TestResult{
		JobID:      job.ID,
		Project:    job.Project,
		Details:    job.Details,
		Priority:   job.Priority,
		EnqueuedAt: job.EnqueuedAt,
		StartedAt:  job.StartedAt,
	}

	if scriptCommandStr, ok := job.Details["script_command"].(string); ok {
		parts := strings.Fields(scriptCommandStr)
		if len(parts) == 0 {
			errMsg := "Error: 'script_command' is empty after splitting."
			log.Printf("[%s] %s", job.ID, errMsg)
			result.Status = "ERROR"
			result.Logs = errMsg
			return result, nil, "", fmt.Errorf(errMsg)
		}

		command := parts[0]
		args := []string{}
		if len(parts) > 1 {
			args = parts[1:]
		}

		innerDetailsJSON, errMarshal := json.Marshal(job.Details)
		if errMarshal != nil {
			log.Printf("[%s] Error marshalling inner details to JSON: %v", job.ID, errMarshal)
			return nil, nil, "", fmt.Errorf("failed to marshal inner details: %w", errMarshal)
		}
		args = append(args, string(innerDetailsJSON))
		
		cmd := exec.Command(command, args...)
		if executionDir != "" {
			cmd.Dir = executionDir
			log.Printf("[%s] Executing command '%s' with args %v in directory: %s", job.ID, command, args, executionDir)
		} else {
			log.Printf("[%s] Executing command '%s' with args %v in runner's working directory.", job.ID, command, args)
		}

		if runtime.GOOS != "windows" {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		var stdoutBuffer, stderrBuffer bytes.Buffer
		cmd.Stdout = &stdoutBuffer
		cmd.Stderr = &stderrBuffer

		if err := cmd.Start(); err != nil {
			errMsg := fmt.Sprintf("Error starting script command: %v", err)
			log.Printf("[%s] %s", job.ID, errMsg)
			result.Status = "ERROR"
			result.Logs = errMsg
			return result, nil, "", fmt.Errorf(errMsg)
		}

		log.Printf("[%s] Script process started with PID %d.", job.ID, cmd.Process.Pid)

		cmdDone := make(chan error, 1)
		go func() {
			cmdDone <- cmd.Wait()
		}()
		
		var commandErr error

		select {
		case <-ctx.Done():
			log.Printf("[%s] Context cancelled. Sending termination signal to process group %d...", job.ID, cmd.Process.Pid)
			killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if killErr != nil {
				log.Printf("[%s] Failed to send SIGTERM to process group: %v. Attempting forceful kill.", job.ID, killErr)
				cmd.Process.Kill() // Fallback to forceful kill
			} else {
				log.Printf("[%s] SIGTERM sent successfully.", job.ID)
			}
			commandErr = <-cmdDone 
			log.Printf("[%s] Process terminated after cancellation with error: %v", job.ID, commandErr)
			
			// After aborting, we still need to process the buffers to find attachment paths.
			stdoutOutput := stdoutBuffer.Bytes()
			stderrOutput := stderrBuffer.String()
			fullLogs := fmt.Sprintf("STDERR:\n%s", stderrOutput)

			// Set the definitive aborted status.
			result.Status = "ABORTED"
			result.Messages = append(result.Messages, "Job was aborted by runner.", fullLogs)

			// Attempt to parse the partial output to salvage attachment info.
			if len(stdoutOutput) > 0 {
				if errUnmarshal := json.Unmarshal(stdoutOutput, result); errUnmarshal == nil {
					log.Printf("[%s] Successfully parsed partial report from aborted script to find attachments.", job.ID)
					
					// Log file
					if strings.TrimSpace(result.Logs) != "" {
						logPath := strings.TrimSpace(result.Logs)
						fullPath := logPath
						if executionDir != "" {
							fullPath = filepath.Join(executionDir, logPath)
						}
						attachments = append(attachments, FileAttachment{FieldName: "log_file", FilePath: fullPath})
						log.Printf("[%s] Found log file attachment in aborted report: %s", job.ID, fullPath)
					}

					// Screenshots
					if len(result.Screenshots) > 0 {
						for _, screenshotPath := range result.Screenshots {
							p := strings.TrimSpace(screenshotPath)
							if p != "" {
								fullPath := p
								if executionDir != "" {
									fullPath = filepath.Join(executionDir, p)
								}
								attachments = append(attachments, FileAttachment{FieldName: "screenshots", FilePath: fullPath})
								log.Printf("[%s] Found screenshot attachment in aborted report: %s", job.ID, fullPath)
							}
						}
					}
					// Videos
					if len(result.Videos) > 0 {
						for _, videoPath := range result.Videos {
							p := strings.TrimSpace(videoPath)
							if p != "" {
								fullPath := p
								if executionDir != "" {
									fullPath = filepath.Join(executionDir, p)
								}
								attachments = append(attachments, FileAttachment{FieldName: "videos", FilePath: fullPath})
								log.Printf("[%s] Found video attachment in aborted report: %s", job.ID, fullPath)
							}
						}
					}

				} else {
					log.Printf("[%s] Could not parse partial JSON from aborted script: %v", job.ID, errUnmarshal)
				}
			}
		
			// Return the aborted result with any attachments we found.
			return result, attachments, fullLogs, context.Canceled


		case err := <-cmdDone:
			log.Printf("[%s] Script process finished naturally.", job.ID)
			commandErr = err
		}
		
		stdoutOutput := stdoutBuffer.Bytes()
		stderrOutput := stderrBuffer.String()
		fullLogs := ""

		if len(stdoutOutput) > 0 {
			log.Printf("[%s] Script STDOUT:\n%s", job.ID, string(stdoutOutput))
			fullLogs += fmt.Sprintf("STDOUT:\n%s\n", string(stdoutOutput))
		}
		if len(stderrOutput) > 0 {
			log.Printf("[%s] Script STDERR:\n%s", job.ID, stderrOutput)
			fullLogs += fmt.Sprintf("STDERR:\n%s\n", stderrOutput)
		}
		
		if len(stdoutOutput) > 0 {
			// Attempt to unmarshal the script's output into the result struct.
			// We overwrite the existing 'result' object with the more detailed one from the script.
			errUnmarshal := json.Unmarshal(stdoutOutput, result)
			if errUnmarshal != nil {
				log.Printf("[%s] Warning: Failed to unmarshal script STDOUT as JSON: %v. Output will be treated as plain logs.", job.ID, errUnmarshal)
				// If parsing fails but the script exited with code 0, it's ambiguous. We can call it FAILED.
				if commandErr == nil {
					result.Status = "ERROR"
					result.Messages = append(result.Messages, "Script finished successfully but STDOUT was not valid JSON.")
				}
			} else {
				logPathFromScriptOutput = result.Logs
				log.Printf("[%s] Successfully unmarshalled script output. Script-provided status: '%s'", job.ID, result.Status)
			}
		}
		
		// Always append the raw logs for debugging purposes
		result.Messages = append(result.Messages, fullLogs)
		
		if len(result.Screenshots) > 0 {
			for _, screenshotPath := range result.Screenshots {
				p := strings.TrimSpace(screenshotPath)
				if p != "" {
					fullPath := p
					if executionDir != "" {
						fullPath = filepath.Join(executionDir, p)
					}
					attachments = append(attachments, FileAttachment{FieldName: "screenshots", FilePath: fullPath})
					log.Printf("[%s] Added screenshot attachment: %s", job.ID, fullPath)
				}
			}
		}
		if len(result.Videos) > 0 {
			for _, videoPath := range result.Videos {
				p := strings.TrimSpace(videoPath)
				if p != "" {
					fullPath := p
					if executionDir != "" {
						fullPath = filepath.Join(executionDir, p)
					}
					attachments = append(attachments, FileAttachment{FieldName: "videos", FilePath: fullPath})
					log.Printf("[%s] Added video attachment: %s", job.ID, fullPath)
				}
			}
		}
		if strings.TrimSpace(logPathFromScriptOutput) != "" {
			logPath := strings.TrimSpace(logPathFromScriptOutput)
			fullPath := logPath
			if executionDir != "" {
				fullPath = filepath.Join(executionDir, logPath)
			}
			attachments = append(attachments, FileAttachment{FieldName: "log_file", FilePath: fullPath})
			log.Printf("[%s] Added log file attachment from script's 'logs' field: %s", job.ID, fullPath)
		}
		
		if commandErr != nil {
			if result.Status == "" || !isTerminalStatus(result.Status) {
				result.Status = "FAILED"
			}
			result.Messages = append(result.Messages, fmt.Sprintf("Script execution failed with error: %v", commandErr))
			return result, attachments, fullLogs, commandErr
		}

		// If we get here with no error and no status, mark as completed.
		if result.Status == "" {
			result.Status = "COMPLETED"
		}
		return result, attachments, fullLogs, nil

	} else {
		errMsg := "Error: 'script_command' not found, is empty, or is not a string."
		log.Printf("[%s] %s", job.ID, errMsg)
		result.Status = "ERROR"
		result.Messages = append(result.Messages, errMsg)
		return result, nil, "", fmt.Errorf(errMsg)
	}
}

func getCurrentJobStatus(apiBaseURL, jobID string) (string, error) {
	url := fmt.Sprintf("%s/jobs/%s/status-check", apiBaseURL, jobID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for job details: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body) // Try to read body for more error info
		return "", fmt.Errorf("api request for job status check failed with status %s: %s", resp.Status, string(bodyBytes))
	}

	var statusResponse JobStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResponse); err != nil {
		return "", fmt.Errorf("failed to decode job status response: %w", err)
	}

	return statusResponse.Status, nil

}

// getJobResultDetails fetches the current result data for a given job ID.
func getJobResultDetails(apiBaseURL, jobID string) (*TestResult, error) {
	log.Printf("Called getJobResultDetails with id %s", jobID)

	url := fmt.Sprintf("%s/results/%s", apiBaseURL, jobID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for job result details: %w", err)
	}
	req.Header.Add("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		log.Printf("No current result details found for job %s (status %d), no backup will be made.", jobID, resp.StatusCode)
		return nil, nil // No data to backup, not an error for the backup logic itself.
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api request for job result details failed with status %s: %s", resp.Status, string(bodyBytes))
	}

	var resultData TestResult
	if err := json.NewDecoder(resp.Body).Decode(&resultData); err != nil {
		return nil, fmt.Errorf("failed to decode job result details response: %w", err)
	}
	return &resultData, nil
}

// postJobResultBackup posts the job result data and any associated files as a multipart form.
func postJobResultBackup(apiBaseURL, jobID string, resultData *TestResult, attachments []FileAttachment) error {
	url := fmt.Sprintf("%s/results/%s", apiBaseURL, jobID)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	resultJSON, err := json.Marshal(resultData)
	if err != nil {
		return fmt.Errorf("failed to marshal result data to JSON: %w", err)
	}

	formField, err := writer.CreateFormField("result")
	if err != nil {
		return fmt.Errorf("failed to create form field 'result': %w", err)
	}
	_, err = formField.Write(resultJSON)
	if err != nil {
		return fmt.Errorf("failed to write result JSON to form field: %w", err)
	}

	// Add file attachments
	for _, fa := range attachments {
		file, err := os.Open(fa.FilePath)
		if err != nil {
			// Log and continue? Or fail hard? For backup, maybe log and continue.
			log.Printf("Warning: failed to open file %s for field %s: %v. Skipping this file.", fa.FilePath, fa.FieldName, err)
			continue
		}
		defer file.Close() // Ensure file is closed

		part, err := writer.CreateFormFile(fa.FieldName, filepath.Base(fa.FilePath)) // Use only the base filename to prevent path issues
		if err != nil {
			return fmt.Errorf("failed to create form file for %s (%s): %w", fa.FieldName, fa.FilePath, err)
		}
		if _, err = io.Copy(part, file); err != nil {
			return fmt.Errorf("failed to copy file content for %s (%s): %w", fa.FieldName, fa.FilePath, err)
		}
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return fmt.Errorf("failed to create request for posting job result backup: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	// Typically, a successful POST might return 200 OK or 201 Created.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request for posting job result backup failed with status %s: %s", resp.Status, string(bodyBytes))
	}

	log.Printf("Successfully posted job result backup for job %s. Server response status: %s", jobID, resp.Status)
	return nil
}

