// Package cleaner provides automatic TTL-based cleanup of temporary sessions,
// uploaded PDFs, and generated split/zip files for the PDF Splitter application.
// It tracks session creation timestamps and purges data exceeding the retention period.
package cleaner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	return os.WriteFile(filepath.Join(sessionDir, "meta.json"), data, 0600)
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
		return time.Unix(meta.CreatedAtUnix, 0).UTC(), true
	}
	return time.Time{}, false
}

// StartCleaner runs a background goroutine that periodically scans the given
// base directory for session folders older than maxAge and removes them.
// If onPurge is non-nil, it is called with each purged session ID so callers
// can evict in-memory session state.
func StartCleaner(ctx context.Context, sessionsDir string, maxAge time.Duration, interval time.Duration, onPurge func(sessionID string)) {
	if maxAge <= 0 {
		return
	}
	if interval <= 0 {
		interval = 1 * time.Minute
	}

	// Immediate run on startup: inspect all existing sessions for stale leftovers.
	cleanExpiredSessions(sessionsDir, maxAge, onPurge)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanExpiredSessions(sessionsDir, maxAge, onPurge)
			}
		}
	}()
}

// cleanExpiredSessions checks every session folder against maxAge.
// It uses meta.json to determine when the session was created, falling back to directory ModTime.
func cleanExpiredSessions(sessionsDir string, maxAge time.Duration, onPurge func(sessionID string)) {
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		slog.Warn("failed to read sessions directory", "dir", sessionsDir, "err", err)
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		sessionID := entry.Name()
		sessionPath := filepath.Join(sessionsDir, sessionID)

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
			if err := os.RemoveAll(sessionPath); err != nil {
				slog.Warn("failed to delete expired session", "session_id", sessionID, "err", err)
				continue
			}
			if onPurge != nil {
				onPurge(sessionID)
			}
			slog.Info("purged expired session", "session_id", sessionID, "age", age.Round(time.Second), "retention", maxAge)
		}
	}
}

// CleanDirRemover deletes a specific path and wraps any failure.
func CleanDirRemover(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	return nil
}

// PurgeAllSessions immediately wipes all session entries inside sessionsDir
// while preserving sessionsDir itself (important under systemd ProtectSystem=strict).
func PurgeAllSessions(sessionsDir string) error {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var firstErr error
	for _, entry := range entries {
		if entry.Name() == ".gitkeep" {
			continue
		}
		target := filepath.Join(sessionsDir, entry.Name())
		if err := os.RemoveAll(target); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
