package models

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type TestResult struct {
	ID            string          `json:"id"`
	ExecutionID   string          `json:"execution_id"`
	SuiteID       string          `json:"suite_id"`
	ClientID      string          `json:"client_id"`
	Status        string          `json:"status"`
	StartTime     time.Time       `json:"start_time"`
	EndTime       time.Time       `json:"end_time"`
	PassedTests   int             `json:"passed_tests"`
	FailedTests   int             `json:"failed_tests"`
	SkippedTests  int             `json:"skipped_tests"`
	Details       json.RawMessage `json:"details"`
	ArtifactsURLs json.RawMessage `json:"artifacts_urls"`
}

func (r *TestResult) Create(db *sql.DB) error {
	query := `
        INSERT INTO test_results 
            (execution_id, suite_id, client_id, status, start_time, end_time, 
             passed_tests, failed_tests, skipped_tests, details, artifacts_urls)
        VALUES 
            ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        RETURNING id
    `

	return db.QueryRow(
		query,
		r.ExecutionID,
		r.SuiteID,
		r.ClientID,
		r.Status,
		r.StartTime,
		r.EndTime,
		r.PassedTests,
		r.FailedTests,
		r.SkippedTests,
		r.Details,
		r.ArtifactsURLs,
	).Scan(&r.ID)
}

func GetTestResult(db *sql.DB, resultID string) (TestResult, error) {
	var result TestResult

	query := `SELECT 
                id, execution_id, suite_id, client_id, status, 
                start_time, end_time, passed_tests, failed_tests, 
                skipped_tests, details, artifacts_urls 
              FROM test_results WHERE id = $1`

	err := db.QueryRow(query, resultID).Scan(
		&result.ID,
		&result.ExecutionID,
		&result.SuiteID,
		&result.ClientID,
		&result.Status,
		&result.StartTime,
		&result.EndTime,
		&result.PassedTests,
		&result.FailedTests,
		&result.SkippedTests,
		&result.Details,
		&result.ArtifactsURLs,
	)

	return result, err
}

func ListTestResults(db *sql.DB, limit, offset int, suiteID, clientID, status string) ([]TestResult, error) {
	// Build query with filters
	query := `SELECT 
                id, execution_id, suite_id, client_id, status, 
                start_time, end_time, passed_tests, failed_tests, 
                skipped_tests, details, artifacts_urls 
              FROM test_results 
              WHERE 1=1`

	params := []interface{}{}
	paramCounter := 1

	if suiteID != "" {
		query += fmt.Sprintf(" AND suite_id = $%d", paramCounter)
		params = append(params, suiteID)
		paramCounter++
	}

	if clientID != "" {
		query += fmt.Sprintf(" AND client_id = $%d", paramCounter)
		params = append(params, clientID)
		paramCounter++
	}

	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", paramCounter)
		params = append(params, status)
		paramCounter++
	}

	query += " ORDER BY start_time DESC"

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", paramCounter)
		params = append(params, limit)
		paramCounter++
	}

	if offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", paramCounter)
		params = append(params, offset)
	}

	rows, err := db.Query(query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TestResult
	for rows.Next() {
		var result TestResult
		err := rows.Scan(
			&result.ID,
			&result.ExecutionID,
			&result.SuiteID,
			&result.ClientID,
			&result.Status,
			&result.StartTime,
			&result.EndTime,
			&result.PassedTests,
			&result.FailedTests,
			&result.SkippedTests,
			&result.Details,
			&result.ArtifactsURLs,
		)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	return results, nil
}

func UpdateExecutionStatus(db *sql.DB, executionID, status string) error {
	query := `UPDATE test_executions SET status = $1, end_time = $2 WHERE id = $3`
	_, err := db.Exec(query, status, time.Now(), executionID)
	return err
}
