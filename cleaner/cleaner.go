// Package cleaner provides automatic TTL-based cleanup of temporary sessions,
// uploaded PDFs, and generated split/zip files for the Pdf Splitter application.
// It tracks session creation timestamps and purges data exceeding the retention period (15 minutes).
package cleaner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// SessionMeta tracks metadata about when a session was initialized.
type SessionMeta struct {
	SessionID        string    `json:"session_id"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedAtUnix    int64     `json:"created_at_unix"`
	CreatedAtRFC3339 string    `json:"created_at_rfc3339"`
}

// WriteSessionMeta records the exact creation timestamp for a session in meta.json.
func WriteSessionMeta(sessionDir string, sessionID string) error {
	now := time.Now().UTC()
	meta := SessionMeta{
		SessionID:        sessionID,
		CreatedAt:        now,
		CreatedAtUnix:    now.Unix(),
		CreatedAtRFC3339: now.Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sessionDir, "meta.json"), data, 0644)
}

// ReadSessionMeta retrieves the tracked creation timestamp of a session.
// Returns (createdAt, true) if meta.json exists and is valid, or (zero, false) otherwise.
func ReadSessionMeta(sessionDir string) (time.Time, bool) {
	metaPath := filepath.Join(sessionDir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return time.Time{}, false
	}
	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return time.Time{}, false
	}
	if !meta.CreatedAt.IsZero() {
		return meta.CreatedAt, true
	}
	if meta.CreatedAtUnix > 0 {
		return time.Unix(meta.CreatedAtUnix, 0), true
	}
	return time.Time{}, false
}

// StartCleaner runs a background goroutine that periodically scans the given
// base directory for session folders older than maxAge and removes them.
// It also executes an initial check immediately upon startup.
func StartCleaner(ctx context.Context, sessionsDir string, maxAge time.Duration, interval time.Duration) {
	if maxAge <= 0 {
		return // Retention disabled
	}
	if interval <= 0 {
		interval = 1 * time.Minute
	}

	// Immediate run on startup: inspect all existing sessions to see if stale
	cleanExpiredSessions(sessionsDir, maxAge)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanExpiredSessions(sessionsDir, maxAge)
			}
		}
	}()
}

// cleanExpiredSessions checks every session folder against maxAge.
// It uses meta.json to determine when the session was created, falling back to directory ModTime.
func cleanExpiredSessions(sessionsDir string, maxAge time.Duration) {
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		log.Printf("[CLEANER] Failed to read sessions directory %s: %v", sessionsDir, err)
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		sessionPath := filepath.Join(sessionsDir, entry.Name())

		// Read creation timestamp from meta.json if available
		createdTime, ok := ReadSessionMeta(sessionPath)
		if !ok {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			createdTime = info.ModTime()
		}

		age := now.Sub(createdTime)
		if age > maxAge {
			log.Printf("[CLEANER] Removing expired session %s (created %v ago > retention %v)", entry.Name(), age.Round(time.Second), maxAge)
			if err := os.RemoveAll(sessionPath); err != nil {
				log.Printf("[CLEANER] Failed to delete session %s: %v", sessionPath, err)
			} else {
				log.Printf("[CLEANER] Successfully purged expired session %s", entry.Name())
			}
		} else {
			remaining := maxAge - age
			log.Printf("[CLEANER] Session %s is within retention (age: %v, remaining: %v)", entry.Name(), age.Round(time.Second), remaining.Round(time.Second))
		}
	}
}

// CleanDirRemover deletes a specific path and logs any failure.
func CleanDirRemover(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	return nil
}

// PurgeAllSessions immediately wipes all session directories under sessionsDir.
func PurgeAllSessions(sessionsDir string) error {
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(sessionsDir)
}
