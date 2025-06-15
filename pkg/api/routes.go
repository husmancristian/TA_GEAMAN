package api

import (
	"net/http"

	"github.com/husmancristian/TA_GEAMAN/pkg/config"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors" // Import CORS package
)

// SetupRouter initializes the Chi router and defines the API endpoints.
func SetupRouter(api *API, cfg *config.Config) http.Handler {
	r := chi.NewRouter()

	// --- CORS Configuration ---
	// Setup CORS middleware with permissive options for development
	// For production, restrict AllowedOrigins more tightly.
	corsMiddleware := cors.New(cors.Options{
		// AllowedOrigins: []string{"http://localhost:5000", "http://127.0.0.1:5000"}, // Example: Allow specific Flutter dev ports
		AllowedOrigins:   []string{"*"},                                                       // Allow requests from any origin (most permissive, use carefully)
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},                 // Methods your API uses
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"}, // Headers your client might send
		ExposedHeaders:   []string{"Link"},                                                    // Headers your server might expose
		AllowCredentials: true,                                                                // Allow cookies, if needed (often not for simple APIs)
		MaxAge:           300,                                                                 // Maximum value not ignored by any of major browsers
	})

	// --- Standard Middleware Stack ---
	r.Use(corsMiddleware.Handler) // Apply CORS middleware FIRST or early
	r.Use(middleware.RequestID)   // Assign unique request IDs
	r.Use(middleware.RealIP)      // Get real client IP
	// Replace chi's default logger with our custom structured logger
	r.Use(StructuredRequestLogger(api.Logger)) // Use the logger from the API struct
	r.Use(middleware.Recoverer)                // Recover from panics
	// Set a reasonable timeout for all requests using config
	r.Use(middleware.Timeout(cfg.RequestTimeout))

	// Basic health check endpoint (doesn't need API struct)
	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	// API v1 Routes (Grouped under /api/v1)
	r.Route("/api/v1", func(r chi.Router) {
		// Test Job Submission & Retrieval
		r.Route("/tests", func(r chi.Router) {
			r.Post("/", api.HandleEnqueueTest)              // Enqueue a new test job
			r.Get("/{project}/next", api.HandleGetNextTest) // Get the next job for a project
		})

		// Test Result Submission & Retrieval
		r.Route("/results", func(r chi.Router) {
			// Expects multipart/form-data now
			r.Post("/{jobId}", api.HandleSubmitResult) // Submit results for a completed job
			r.Get("/{jobId}", api.HandleGetResult)     // Get results for a specific job
		})

		// Job Management Actions & Listing
		r.Route("/jobs", func(r chi.Router) {
			// New endpoint to list jobs
			r.Get("/", api.HandleGetJobs)

			// Actions on specific jobs
			r.Route("/{jobId}", func(r chi.Router) {
				r.Post("/cancel", api.HandleCancelJob)           // Request cancellation of a pending job
				r.Post("/skip", api.HandleSkipJob)               // Mark a pending job as skipped
				r.Post("/rerun", api.HandleRerunJob)             // Re-queue a completed/failed job
				r.Post("/abort", api.HandleAbortJob)             // Request abortion of a running job
				r.Post("/progress", api.HandleUpdateJobProgress) // Update job progress in real-time
				// Maybe add: r.Get("/status", api.HandleGetJobStatus) // Get current status without full result
			})
		})

		// Queue Status
		r.Route("/queues", func(r chi.Router) {
			// New endpoint for overview of all queues
			r.Get("/overview", api.HandleGetAllQueueStatuses)
			// Existing endpoint for specific queue
			r.Get("/{project}/status", api.HandleGetQueueStatus)
		})

		// Project Management & Project-Specific Data
		r.Route("/projects", func(r chi.Router) {
			r.Post("/", api.HandleAddProject) // Add a new project
			r.Route("/{projectName}", func(r chi.Router) {
				r.Delete("/", api.HandleDeleteProject)         // Delete a project
				r.Get("/results", api.HandleGetProjectResults) // Get all results for a specific project (uses projectName)
			})
		})
	})

	return r
}
