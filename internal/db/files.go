package db

import (
	"database/sql"
	"time"
)

type FileState string

const (
	FileDiscovered FileState = "discovered"
	FileCopied     FileState = "copied"
	FileQueued     FileState = "queued"
	FileUploading  FileState = "uploading"
	FileUploaded   FileState = "uploaded"
	FileFailed     FileState = "failed"
	FileAbandoned  FileState = "abandoned"
	FileDeleted    FileState = "deleted"
)

type File struct {
	ID             int64
	SessionID      int64
	Filename       string
	SizeBytes      int64
	ChecksumSHA256 string
	State          FileState
	DiscoveredAt   time.Time
	CopiedAt       *time.Time
	QueuedAt       *time.Time
	UploadedAt     *time.Time
	DeletedAt      *time.Time
	UploadAttempts int
	DestinationID  *int64
	RemotePath     *string
	ErrorMessage   *string
}

func (d *DB) InsertFile(f *File) (int64, error) {
	res, err := d.conn.Exec(`
		INSERT INTO files
			(session_id, filename, size_bytes, checksum_sha256, state, updated_at)
		VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%d %H:%M:%f','now'))`,
		f.SessionID, f.Filename, f.SizeBytes, f.ChecksumSHA256, f.State,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetFileByChecksum(checksum string) (*File, error) {
	f := &File{}
	err := d.conn.QueryRow(`
		SELECT id, session_id, filename, size_bytes, checksum_sha256,
		       state, discovered_at, upload_attempts
		FROM files WHERE checksum_sha256 = ?
		ORDER BY id DESC LIMIT 1`, checksum,
	).Scan(
		&f.ID, &f.SessionID, &f.Filename, &f.SizeBytes,
		&f.ChecksumSHA256, &f.State, &f.DiscoveredAt, &f.UploadAttempts,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (d *DB) UpdateFileState(id int64, state FileState) error {
	var col string
	switch state {
	case FileCopied:
		col = ", copied_at = CURRENT_TIMESTAMP"
	case FileQueued:
		col = ", queued_at = CURRENT_TIMESTAMP"
	case FileUploaded:
		col = ", uploaded_at = CURRENT_TIMESTAMP"
	case FileDeleted:
		col = ", deleted_at = CURRENT_TIMESTAMP"
	default:
		col = ""
	}

	_, err := d.conn.Exec(
		`UPDATE files SET state = ?, updated_at = strftime('%Y-%m-%d %H:%M:%f','now')`+col+` WHERE id = ?`,
		state, id,
	)
	return err
}

func (d *DB) UpdateFileError(id int64, msg string) error {
	_, err := d.conn.Exec(`
		UPDATE files SET
			error_message = ?,
			upload_attempts = upload_attempts + 1,
			updated_at = strftime('%Y-%m-%d %H:%M:%f','now')
		WHERE id = ?`, msg, id,
	)
	return err
}

func (d *DB) UpdateFileUploaded(id int64, destID int64, remotePath string) error {
	_, err := d.conn.Exec(`
		UPDATE files SET
			state = ?,
			uploaded_at = CURRENT_TIMESTAMP,
			updated_at = strftime('%Y-%m-%d %H:%M:%f','now'),
			destination_id = ?,
			remote_path = ?
		WHERE id = ?`,
		FileUploaded, destID, remotePath, id,
	)
	return err
}

func (d *DB) GetQueuedFiles() ([]File, error) {
	rows, err := d.conn.Query(`
		SELECT id, session_id, filename, size_bytes, checksum_sha256,
		       state, discovered_at, upload_attempts
		FROM files
		WHERE state IN (?, ?)
		ORDER BY discovered_at ASC`,
		FileQueued, FileFailed,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var f File
		err := rows.Scan(
			&f.ID, &f.SessionID, &f.Filename, &f.SizeBytes,
			&f.ChecksumSHA256, &f.State, &f.DiscoveredAt, &f.UploadAttempts,
		)
		if err != nil {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

// FileChange is a lightweight, portal-facing view of a file used for the delta
// sync. Timestamps are kept as strings to avoid driver datetime parsing.
type FileChange struct {
	Checksum       string `json:"hash"` // content SHA-256 — the stable unique id
	Filename       string `json:"name"`
	SizeBytes      int64  `json:"sizeBytes"`
	State          string `json:"state"`
	UploadAttempts int    `json:"uploadAttempts"`
	ErrorMessage   string `json:"error,omitempty"`
	DiscoveredAt   string `json:"discoveredAt,omitempty"`
	UploadedAt     string `json:"uploadedAt,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

// GetFilesChangedSince returns files whose updated_at is strictly greater than
// watermark, oldest change first, capped at limit. The caller sends these to
// the portal and advances its watermark to the newest UpdatedAt on success.
func (d *DB) GetFilesChangedSince(watermark string, limit int) ([]FileChange, error) {
	rows, err := d.conn.Query(`
		SELECT checksum_sha256, filename, size_bytes, state, upload_attempts,
		       COALESCE(error_message, ''), COALESCE(discovered_at, ''),
		       COALESCE(uploaded_at, ''), COALESCE(updated_at, '')
		FROM files
		WHERE updated_at > ?
		ORDER BY updated_at ASC
		LIMIT ?`, watermark, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var changes []FileChange
	for rows.Next() {
		var c FileChange
		if err := rows.Scan(&c.Checksum, &c.Filename, &c.SizeBytes, &c.State,
			&c.UploadAttempts, &c.ErrorMessage, &c.DiscoveredAt,
			&c.UploadedAt, &c.UpdatedAt); err != nil {
			continue
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

func (d *DB) GetUploadedFilesEligibleForDeletion() ([]File, error) {
	rows, err := d.conn.Query(`
		SELECT id, filename, size_bytes, uploaded_at
		FROM files
		WHERE state = ?
		ORDER BY uploaded_at ASC`,
		FileUploaded,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var f File
		rows.Scan(&f.ID, &f.Filename, &f.SizeBytes, &f.UploadedAt)
		files = append(files, f)
	}
	return files, nil
}

func (d *DB) ResetStuckFiles() error {
	_, err := d.conn.Exec(`
		UPDATE files SET state = ?
		WHERE state = ?`,
		FileQueued, FileUploading,
	)
	return err
}
