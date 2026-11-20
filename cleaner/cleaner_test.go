package cleaner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionMeta(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "meta_test_session"
	sessionPath := filepath.Join(tmpDir, sessionID)
	if err := os.MkdirAll(sessionPath, 0755); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	// 1. Write session meta
	if err := WriteSessionMeta(sessionPath, sessionID); err != nil {
		t.Fatalf("WriteSessionMeta failed: %v", err)
	}

	// 2. Read session meta
	createdAt, ok := ReadSessionMeta(sessionPath)
	if !ok {
		t.Fatalf("expected ReadSessionMeta to return true")
	}

	if time.Since(createdAt) > 5*time.Second {
		t.Errorf("expected createdAt to be recent, got: %v", createdAt)
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Session A: Stale session (created 20 minutes ago > 15m retention)
	staleSession := "stale_session_1"
	stalePath := filepath.Join(tmpDir, staleSession)
	_ = os.MkdirAll(stalePath, 0755)
	pastTime := time.Now().Add(-20 * time.Minute)
	// Write meta.json with past timestamp
	metaJSON := `{"session_id": "stale_session_1", "created_at_unix": ` +
		time.Now().Add(-20*time.Minute).Format("1732135000") + `, "created_at": "` +
		pastTime.Format(time.RFC3339) + `"}`
	_ = os.WriteFile(filepath.Join(stalePath, "meta.json"), []byte(metaJSON), 0644)

	// Session B: Fresh session (created 5 minutes ago < 15m retention)
	freshSession := "fresh_session_2"
	freshPath := filepath.Join(tmpDir, freshSession)
	_ = os.MkdirAll(freshPath, 0755)
	_ = WriteSessionMeta(freshPath, freshSession)

	// Run cleanExpiredSessions with 15-minute retention
	cleanExpiredSessions(tmpDir, 15*time.Minute)

	// Stale session must be deleted
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale session to be purged")
	}

	// Fresh session must be retained
	if _, err := os.Stat(freshPath); os.IsNotExist(err) {
		t.Errorf("expected fresh session to be retained")
	}
}
