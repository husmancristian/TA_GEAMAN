package httperrors

import (
	"encoding/json"
	"log/slog" // Use slog for logging errors internally
	"net/http"
)

// ErrorResponse defines the standard JSON error structure.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// RespondWithError sends a JSON error response.
func RespondWithError(w http.ResponseWriter, logger *slog.Logger, status int, internalError error, userMessage string) {
	// Log the internal error for debugging
	if internalError != nil {
		logger.Error("API Error",
			slog.Int("status", status),
			slog.String("user_message", userMessage),
			slog.String("internal_error", internalError.Error()),
		)
	} else {
		// Log even if internalError is nil, userMessage might be important
		logger.Warn("API Response Error",
			slog.Int("status", status),
			slog.String("user_message", userMessage),
		)
	}

	// Create the user-facing error response
	errResp := ErrorResponse{
		Error:   http.StatusText(status),
		Message: userMessage,
		Status:  status,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errResp); err != nil {
		// Fallback if JSON encoding fails
		logger.Error("Failed to encode error response", slog.String("encoding_error", err.Error()))
		http.Error(w, `{"error":"Internal Server Error", "message":"Failed to encode error response"}`, http.StatusInternalServerError)
	}
}

// Convenience functions for common errors

func BadRequest(w http.ResponseWriter, logger *slog.Logger, err error, message string) {
	RespondWithError(w, logger, http.StatusBadRequest, err, message)
}

func NotFound(w http.ResponseWriter, logger *slog.Logger, err error, message string) {
	RespondWithError(w, logger, http.StatusNotFound, err, message)
}

func InternalServerError(w http.ResponseWriter, logger *slog.Logger, err error, message string) {
	if message == "" {
		message = "An unexpected error occurred."
	}
	RespondWithError(w, logger, http.StatusInternalServerError, err, message)
}

func StatusNotImplemented(w http.ResponseWriter, logger *slog.Logger, err error, message string) {
	if message == "" {
		message = "This feature is not yet implemented."
	}
	RespondWithError(w, logger, http.StatusNotImplemented, err, message)
}
