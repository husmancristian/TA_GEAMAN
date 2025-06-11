// pkg/db/models/models.go
package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

type TestClient struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	IPAddress  string          `json:"description"`
	Config     json.RawMessage `json:"config"`
	Platform   time.Time       `json:"platform"`
	Status     string          `json:"status"`
	LastSeen   time.Time       `json:"lastSeen"`
	Attributes time.Time       `json:"attributes"`
}

func GetTestClient(db *sql.DB, clientID string) (TestClient, error) {
	var client TestClient

	query := `SELECT id, name, ip_address, platform, status, last_seen, attributes 
              FROM test_clients WHERE id = $1`

	err := db.QueryRow(query, clientID).Scan(
		&client.ID,
		&client.Name,
		&client.IPAddress,
		&client.Platform,
		&client.Status,
		&client.LastSeen,
		&client.Attributes,
	)

	if err != nil {
		return TestClient{}, err
	}

	return client, nil
}

func (t TestClient) Update(db *sql.DB) error {
	query := `UPDATE test_clients 
              SET name = $1, ip_address = $2, platform = $3, 
                  status = $4, last_seen = $5, attributes = $6
              WHERE id = $7`

	_, err := db.Exec(query,
		t.Name,
		t.IPAddress,
		t.Platform,
		t.Status,
		t.LastSeen,
		t.Attributes,
		t.ID)

	return err
}

func (t TestClient) Upsert(db *sql.DB) error {
	query := `
        INSERT INTO test_clients 
            (id, name, ip_address, platform, status, last_seen, attributes)
        VALUES 
            ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (id) DO UPDATE SET
            name = $2,
            ip_address = $3,
            platform = $4,
            status = $5,
            last_seen = $6,
            attributes = $7
    `

	_, err := db.Exec(query,
		t.ID,
		t.Name,
		t.IPAddress,
		t.Platform,
		t.Status,
		t.LastSeen,
		t.Attributes)

	return err
}
