package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

const defaultTagLimit = 100

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "/app/db")
	if err != nil {
		log.Fatalf("Failed to open database: %s", err.Error())
	}

	createTagsTable := `CREATE TABLE IF NOT EXISTS tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_by INTEGER
	)`
	if _, err := db.Exec(createTagsTable); err != nil {
		log.Fatalf("Failed to create tags table: %s", err.Error())
	}

	createTagImageTable := `CREATE TABLE IF NOT EXISTS tag_images (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tag_id INTEGER NOT NULL,
		file_name TEXT NOT NULL,
		FOREIGN KEY(tag_id) REFERENCES tags(id)
	)`
	if _, err := db.Exec(createTagImageTable); err != nil {
		log.Fatalf("Failed to create tag_images table: %s", err.Error())
	}

	createTagLimitsTable := `CREATE TABLE IF NOT EXISTS tag_limits (
		user_id INTEGER PRIMARY KEY,
		tag_limit INTEGER NOT NULL DEFAULT 100
	)`
	if _, err := db.Exec(createTagLimitsTable); err != nil {
		log.Fatalf("Failed to create tag_limits table: %s", err.Error())
	}

	return db, nil
}

func tagLimit(q rowQuerier, userID int64) (int, error) {
	var limit int
	err := q.QueryRow("SELECT tag_limit FROM tag_limits WHERE user_id = ?", userID).Scan(&limit)
	if err == sql.ErrNoRows {
		return defaultTagLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to query tag limit: %s", err.Error())
	}
	return limit, nil
}

func queryTagNames(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query tags: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read tags: %s", err.Error())
	}

	return tags, nil
}

func tagsCaption(db *sql.DB, fileName string) (string, error) {
	rows, err := db.Query(
		"SELECT DISTINCT t.name FROM tags t JOIN tag_images ti ON ti.tag_id = t.id WHERE ti.file_name = ?",
		fileName,
	)
	if err != nil {
		return "", fmt.Errorf("failed to query tags for image: %s", err.Error())
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("failed to scan tag name: %s", err.Error())
		}
		tags = append(tags, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("failed to read tags for image: %s", err.Error())
	}

	return strings.Join(tags, ", "), nil
}
