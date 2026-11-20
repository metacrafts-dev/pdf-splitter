package cleaner

import (
	"fmt"
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

	if err := WriteSessionMeta(sessionPath, sessionID); err != nil {
		t.Fatalf("WriteSessionMeta failed: %v", err)
	}

	createdAt, ok := ReadSessionMeta(sessionPath)
	if !ok {
		t.Fatalf("expected ReadSessionMeta to return true")
	}

	if time.Since(createdAt) > 5*time.Second {
		t.Errorf("expected createdAt to be recent, got: %v", createdAt)
	}
}

func TestReadSessionMetaUnixFallback(t *testing.T) {
	tmpDir := t.TempDir()
	pastTime := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	metaJSON := fmt.Sprintf(`{"session_id": "unix_only", "created_at_unix": %d}`, pastTime.Unix())
	if err := os.WriteFile(filepath.Join(tmpDir, "meta.json"), []byte(metaJSON), 0600); err != nil {
		t.Fatalf("failed to write meta.json: %v", err)
	}

	got, ok := ReadSessionMeta(tmpDir)
	if !ok {
		t.Fatalf("expected ReadSessionMeta unix fallback to return true")
	}
	if !got.Equal(pastTime) {
		t.Errorf("expected %v, got %v", pastTime, got)
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Session A: Stale session (created 20 minutes ago > 15m retention)
	staleSession := "stale_session_1"
	stalePath := filepath.Join(tmpDir, staleSession)
	if err := os.MkdirAll(stalePath, 0755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	pastTime := time.Now().UTC().Add(-20 * time.Minute)
	metaJSON := fmt.Sprintf(
		`{"session_id": "%s", "created_at_unix": %d, "created_at": "%s"}`,
		staleSession, pastTime.Unix(), pastTime.Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(stalePath, "meta.json"), []byte(metaJSON), 0600); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}

	// Session B: Fresh session
	freshSession := "fresh_session_2"
	freshPath := filepath.Join(tmpDir, freshSession)
	if err := os.MkdirAll(freshPath, 0755); err != nil {
		t.Fatalf("mkdir fresh: %v", err)
	}
	if err := WriteSessionMeta(freshPath, freshSession); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}

	var purged []string
	cleanExpiredSessions(tmpDir, 15*time.Minute, func(sessionID string) {
		purged = append(purged, sessionID)
	})

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale session to be purged")
	}
	if _, err := os.Stat(freshPath); os.IsNotExist(err) {
		t.Errorf("expected fresh session to be retained")
	}
	if len(purged) != 1 || purged[0] != staleSession {
		t.Errorf("expected onPurge callback for %q, got %v", staleSession, purged)
	}
}

func TestPurgeAllSessionsPreservesRoot(t *testing.T) {
	tmpDir := t.TempDir()
	sub := filepath.Join(tmpDir, "session_abc")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitkeep"), nil, 0644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}

	if err := PurgeAllSessions(tmpDir); err != nil {
		t.Fatalf("PurgeAllSessions failed: %v", err)
	}
	if _, err := os.Stat(tmpDir); err != nil {
		t.Errorf("expected sessionsDir root to still exist: %v", err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Errorf("expected child session dir to be deleted")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".gitkeep")); err != nil {
		t.Errorf("expected .gitkeep to be preserved: %v", err)
	}
}
