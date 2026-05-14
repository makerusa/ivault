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
			(session_id, filename, size_bytes, checksum_sha256, state)
		VALUES (?, ?, ?, ?, ?)`,
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
		`UPDATE files SET state = ?`+col+` WHERE id = ?`,
		state, id,
	)
	return err
}

func (d *DB) UpdateFileError(id int64, msg string) error {
	_, err := d.conn.Exec(`
		UPDATE files SET
			error_message = ?,
			upload_attempts = upload_attempts + 1
		WHERE id = ?`, msg, id,
	)
	return err
}

func (d *DB) UpdateFileUploaded(id int64, destID int64, remotePath string) error {
	_, err := d.conn.Exec(`
		UPDATE files SET
			state = ?,
			uploaded_at = CURRENT_TIMESTAMP,
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
