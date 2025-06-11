package models

import (
	"database/sql"
	"fmt"
	"time"
)

type TestExecution struct {
	ID        string    `json:"id"`
	SuiteID   string    `json:"suite_id"`
	ClientID  string    `json:"client_id"`
	Status    string    `json:"status"` // queued, running, completed, failed, cancelled
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Priority  int       `json:"priority"`
	Metadata  string    `json:"metadata"` // JSON metadata
}

// Create inserts a new test execution record
func (e *TestExecution) Create(db *sql.DB) error {
	query := `
		INSERT INTO test_executions 
			(suite_id, client_id, status, priority, metadata, start_time)
		VALUES 
			($1, $2, $3, $4, $5, $6)
		RETURNING id, start_time
	`

	// Set start time to now if not provided
	if e.StartTime.IsZero() {
		e.StartTime = time.Now()
	}

	return db.QueryRow(
		query,
		e.SuiteID,
		e.ClientID,
		e.Status,
		e.Priority,
		e.Metadata,
		e.StartTime,
	).Scan(&e.ID, &e.StartTime)
}

// SetStatus updates the status of a test execution
func (e *TestExecution) SetStatus(db *sql.DB, status string) error {
	query := `UPDATE test_executions SET status = $1 WHERE id = $2`

	// If we're completing the execution, set the end time
	if status == "completed" || status == "failed" || status == "cancelled" {
		query = `UPDATE test_executions SET status = $1, end_time = $2 WHERE id = $3`
		e.EndTime = time.Now()
		_, err := db.Exec(query, status, e.EndTime, e.ID)
		e.Status = status
		return err
	}

	_, err := db.Exec(query, status, e.ID)
	e.Status = status
	return err
}

// Get retrieves a test execution by ID
func GetTestExecution(db *sql.DB, executionID string) (TestExecution, error) {
	var execution TestExecution

	query := `
		SELECT id, suite_id, client_id, status, 
			   start_time, end_time, priority, metadata
		FROM test_executions
		WHERE id = $1
	`

	err := db.QueryRow(query, executionID).Scan(
		&execution.ID,
		&execution.SuiteID,
		&execution.ClientID,
		&execution.Status,
		&execution.StartTime,
		&execution.EndTime,
		&execution.Priority,
		&execution.Metadata,
	)

	return execution, err
}

// List retrieves test executions with optional filtering
func ListTestExecutions(db *sql.DB, limit, offset int, clientID, status string) ([]TestExecution, error) {
	query := `
		SELECT id, suite_id, client_id, status, 
			   start_time, end_time, priority, metadata
		FROM test_executions
		WHERE 1=1
	`

	args := []interface{}{}
	argCount := 1

	if clientID != "" {
		query += ` AND client_id = $` + fmt.Sprint(argCount)
		args = append(args, clientID)
		argCount++
	}

	if status != "" {
		query += ` AND status = $` + fmt.Sprint(argCount)
		args = append(args, status)
		argCount++
	}

	query += ` ORDER BY start_time DESC`

	if limit > 0 {
		query += ` LIMIT $` + fmt.Sprint(argCount)
		args = append(args, limit)
		argCount++
	}

	if offset > 0 {
		query += ` OFFSET $` + fmt.Sprint(argCount)
		args = append(args, offset)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var executions []TestExecution
	for rows.Next() {
		var execution TestExecution
		err := rows.Scan(
			&execution.ID,
			&execution.SuiteID,
			&execution.ClientID,
			&execution.Status,
			&execution.StartTime,
			&execution.EndTime,
			&execution.Priority,
			&execution.Metadata,
		)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}

	return executions, nil
}

// CancelExecution marks a test execution as cancelled
func CancelExecution(db *sql.DB, executionID string) error {
	query := `UPDATE test_executions SET status = 'cancelled', end_time = $1 WHERE id = $2`
	_, err := db.Exec(query, time.Now(), executionID)
	return err
}
