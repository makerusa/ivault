package db

import "time"

type Session struct {
	ID          int64
	StartedAt   time.Time
	EndedAt     *time.Time
	Status      string
	FilesFound  int
	FilesCopied int
	BytesCopied int64
}

func (d *DB) StartSession() (int64, error) {
	res, err := d.conn.Exec(`
		INSERT INTO sessions (status) VALUES ('active')`,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) EndSession(id int64, filesFound, filesCopied int, bytesCopied int64, status string) error {
	_, err := d.conn.Exec(`
		UPDATE sessions SET
			ended_at = CURRENT_TIMESTAMP,
			status = ?,
			files_found = ?,
			files_copied = ?,
			bytes_copied = ?
		WHERE id = ?`,
		status, filesFound, filesCopied, bytesCopied, id,
	)
	return err
}
