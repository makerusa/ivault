package db

import (
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	// A file-based temp DB (mattn/go-sqlite3 :memory: + WAL can be flaky).
	d, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestGetFilesChangedSinceTracksStateChanges(t *testing.T) {
	d := newTestDB(t)

	sid, err := d.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	id, err := d.InsertFile(&File{SessionID: sid, Filename: "clip0001.mp4", SizeBytes: 100, ChecksumSHA256: "abc123", State: FileDiscovered})
	if err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	// From an empty watermark, the new file must appear with full detail.
	changes, err := d.GetFilesChangedSince("", 100)
	if err != nil {
		t.Fatalf("GetFilesChangedSince: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Checksum != "abc123" || changes[0].Filename != "clip0001.mp4" || changes[0].SizeBytes != 100 {
		t.Fatalf("unexpected change record: %+v", changes[0])
	}
	watermark := changes[0].UpdatedAt

	// Nothing changed since that watermark yet.
	if again, _ := d.GetFilesChangedSince(watermark, 100); len(again) != 0 {
		t.Fatalf("expected no changes at watermark, got %d", len(again))
	}

	// Ensure a distinct millisecond, then a state change must resurface it.
	time.Sleep(5 * time.Millisecond)
	if err := d.UpdateFileState(id, FileUploaded); err != nil {
		t.Fatalf("UpdateFileState: %v", err)
	}
	after, err := d.GetFilesChangedSince(watermark, 100)
	if err != nil {
		t.Fatalf("GetFilesChangedSince after update: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected the state change to resurface the file, got %d", len(after))
	}
	if after[0].State != string(FileUploaded) {
		t.Fatalf("expected state %q, got %q", FileUploaded, after[0].State)
	}
	if after[0].UpdatedAt <= watermark {
		t.Fatalf("updated_at should advance past the watermark: %q <= %q", after[0].UpdatedAt, watermark)
	}
}
