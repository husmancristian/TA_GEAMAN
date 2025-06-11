package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

type TestSuite struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Add methods for database operations
func (s *TestSuite) Create(db *sql.DB) error {
	query := `
        INSERT INTO test_suites (name, description, config) 
        VALUES ($1, $2, $3)
        RETURNING id, created_at, updated_at
    `

	return db.QueryRow(
		query,
		s.Name,
		s.Description,
		s.Config,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
}

func ListTestSuites(db *sql.DB) ([]TestSuite, error) {
	rows, err := db.Query("SELECT id, name, description, config, created_at, updated_at FROM test_suites")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suites []TestSuite
	for rows.Next() {
		var suite TestSuite
		err := rows.Scan(
			&suite.ID,
			&suite.Name,
			&suite.Description,
			&suite.Config,
			&suite.CreatedAt,
			&suite.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		suites = append(suites, suite)
	}

	return suites, nil
}
