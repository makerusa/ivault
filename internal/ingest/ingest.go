package ingest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/makerusa/ivault/internal/db"
	"github.com/makerusa/ivault/internal/provision"
)

// IngestConfig holds the filesystem paths used during ingest.
type IngestConfig struct {
	ImagePath   string // e.g. /nvme/usb_disk.img
	MountPoint  string // e.g. /nvme/ingest
	UploadQueue string // e.g. /nvme/upload_queue
	ConfigPath  string // e.g. /etc/ivault/config.json
}

func Mount(cfg IngestConfig) error {
	device := cfg.ImagePath

	if strings.HasPrefix(device, "/dev/") {
		partition := device
		if _, err := os.Stat(device + "p1"); err == nil {
			partition = device + "p1"
		} else if _, err := os.Stat(device + "1"); err == nil {
			partition = device + "1"
		}

		cmd := exec.Command("mount", partition, cfg.MountPoint)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount raw device failed: %w — %s", err, string(out))
		}
		return nil
	}

	// Image-file backing. Attach via a loop device with partition scanning
	// (-P) so a partitioned image — GPT + exFAT partition, which is what lets
	// macOS mount the drive on the host — exposes its partitions. A legacy
	// partitionless ("superfloppy") image has no pN node and is mounted whole.
	out, err := exec.Command("losetup", "-f", "--show", "-P", cfg.ImagePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup failed: %w — %s", err, string(out))
	}
	loop := strings.TrimSpace(string(out))
	exec.Command("udevadm", "settle").Run()
	target := loop
	for i := 0; i < 20; i++ {
		if _, statErr := os.Stat(loop + "p1"); statErr == nil {
			target = loop + "p1"
			break
		}
		// Partitionless images never grow a pN node; give a partitioned one a
		// short moment for udev to create it, then fall back to the whole loop.
		time.Sleep(50 * time.Millisecond)
	}
	mnt := exec.Command("mount", target, cfg.MountPoint)
	if mo, mErr := mnt.CombinedOutput(); mErr != nil {
		exec.Command("losetup", "-d", loop).Run() // release the loop we just created
		return fmt.Errorf("mount loop failed: %w — %s", mErr, string(mo))
	}
	return nil
}

func Unmount(cfg IngestConfig) error {
	// Capture any loop device backing the image BEFORE unmounting, so we can
	// detach it afterward. `mount -o loop` auto-detached on umount; an explicit
	// losetup (used by Mount for partitioned images) does not, so a leaked loop
	// would keep the backing file open and block the next reformat.
	var loops []string
	if !strings.HasPrefix(cfg.ImagePath, "/dev/") {
		if out, err := exec.Command("losetup", "-j", cfg.ImagePath, "-O", "NAME", "-n").CombinedOutput(); err == nil {
			loops = strings.Fields(string(out))
		}
	}
	exec.Command("umount", cfg.MountPoint).CombinedOutput() // ignore "not mounted" errors
	for _, l := range loops {
		exec.Command("losetup", "-d", l).Run()
	}
	return nil
}

// readVolumeLabel returns the filesystem label (e.g. the exFAT volume name) of
// whatever is mounted at mountPoint, or "" if it can't be determined. Must be
// called while the drive is mounted internally (mid-maintenance).
func readVolumeLabel(mountPoint string) string {
	src, err := exec.Command("findmnt", "-n", "-o", "SOURCE", mountPoint).Output()
	source := strings.TrimSpace(string(src))
	if err != nil || source == "" {
		return ""
	}
	out, err := exec.Command("blkid", "-s", "LABEL", "-o", "value", source).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// diskUsageBytes returns used and total bytes of the filesystem mounted at path.
func diskUsageBytes(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	free := st.Bfree * bsize
	if free > total {
		free = total
	}
	return total - free, total, nil
}

type IngestResult struct {
	FilesFound  int
	FilesCopied int
	BytesCopied int64
	Skipped     int // already-seen (deduped by checksum)

	// Filtered counts, for the per-cycle summary.
	SkippedSystem    int // hidden/OS-metadata files (when skip-system is on)
	SkippedTooSmall  int // below the configured minimum size
	SkippedTooLarge  int // above the configured maximum size
	SkippedExtension int // extension in the ignore blocklist
}

// ingestFilters is the effective, portal-configured ignore rules for a cycle.
// Defaults (used when nothing has been synced yet) ingest everything except
// system files.
type ingestFilters struct {
	skipSystemFiles bool
	exts            map[string]bool // lowercased, no leading dot; extensions the rule applies to
	extInclude      bool            // true = archive ONLY exts (allowlist); false = archive all EXCEPT exts
	minSizeBytes    int64           // ignore files smaller than this (0 = no floor)
	maxSizeBytes    int64           // ignore files larger than this (0 = no cap)
}

// loadFilters reads the ignore-rule settings the heartbeat persisted into the
// config table. Missing/invalid values fall back to permissive defaults.
func loadFilters(database *db.DB) ingestFilters {
	f := ingestFilters{skipSystemFiles: true}
	if v, err := database.GetConfig("filter_skip_system_files"); err == nil && v != "" {
		f.skipSystemFiles = v != "false" && v != "0"
	}
	if v, err := database.GetConfig("filter_skip_files_under_mb"); err == nil && v != "" {
		if mb, e := strconv.ParseFloat(v, 64); e == nil && mb > 0 {
			f.minSizeBytes = int64(mb * 1024 * 1024)
		}
	}
	if v, err := database.GetConfig("filter_skip_files_over_mb"); err == nil && v != "" {
		if mb, e := strconv.ParseFloat(v, 64); e == nil && mb > 0 {
			f.maxSizeBytes = int64(mb * 1024 * 1024)
		}
	}
	if v, err := database.GetConfig("filter_extension_mode"); err == nil && v == "include" {
		f.extInclude = true
	}
	if v, err := database.GetConfig("filter_ignored_extensions"); err == nil && v != "" {
		f.exts = map[string]bool{}
		for _, e := range strings.Split(v, ",") {
			e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
			if e != "" {
				f.exts[e] = true
			}
		}
	}
	return f
}

func Run(cfg IngestConfig, database *db.DB, sessionID int64) (*IngestResult, bool, error) {
	result := &IngestResult{}

	// Run provision check
	provisioned, err := provision.Process(cfg.MountPoint, cfg.ConfigPath, database)
	if err != nil {
		return nil, false, fmt.Errorf("provisioning failed: %w", err)
	}

	// Record the external drive's usage while it's mounted here — the USB host
	// owns the drive the rest of the time, so this is the only point we can
	// statfs the exFAT. The heartbeat backfills these into telemetry so the
	// portal shows real "Virtual Drive" usage instead of 0.
	if used, total, uErr := diskUsageBytes(cfg.MountPoint); uErr == nil && total > 0 {
		_ = database.SetConfig("ext_drive_used_bytes", strconv.FormatUint(used, 10))
		_ = database.SetConfig("ext_drive_total_bytes", strconv.FormatUint(total, 10))
		_ = database.SetConfig("ext_drive_measured_at", time.Now().UTC().Format(time.RFC3339))
	}
	// Capture the real exFAT volume label while mounted (it's the only time we
	// can read it), so the portal can show the actual drive name instead of a
	// stored default. Persisted and backfilled into telemetry by the heartbeat.
	if label := readVolumeLabel(cfg.MountPoint); label != "" {
		_ = database.SetConfig("ext_drive_label", label)
	}

	filters := loadFilters(database)

	err = filepath.WalkDir(cfg.MountPoint, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip directories themselves from being processed as files
		if d.IsDir() {
			return nil
		}

		// Compute relative path to preserve folder structure
		relPath, err := filepath.Rel(cfg.MountPoint, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		// Always drop our own provisioning sentinels — they're consumed
		// separately and must never be uploaded, regardless of any toggle.
		if isProvisionFile(relPath) {
			return nil
		}

		// Skip system files / OS metadata / hidden dirs — gated by the
		// portal's "skip system files" toggle (default on).
		if filters.skipSystemFiles && isSystemFileOrInHiddenFolder(relPath) {
			result.SkippedSystem++
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil // Skip on stat error
		}

		// Size ignore rules: too small / too large.
		if filters.minSizeBytes > 0 && info.Size() < filters.minSizeBytes {
			result.SkippedTooSmall++
			return nil
		}
		if filters.maxSizeBytes > 0 && info.Size() > filters.maxSizeBytes {
			result.SkippedTooLarge++
			return nil
		}

		// Extension rule (empty list = archive everything). In exclude mode the
		// listed extensions are skipped; in include mode only the listed
		// extensions are archived and everything else is skipped.
		if len(filters.exts) > 0 {
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(relPath), "."))
			listed := filters.exts[ext]
			if (filters.extInclude && !listed) || (!filters.extInclude && listed) {
				result.SkippedExtension++
				return nil
			}
		}

		result.FilesFound++

		// Compute source checksum
		checksum, err := checksumFile(path)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", relPath, err)
		}

		// Check if already processed by checksum
		existing, err := database.GetFileByChecksum(checksum)
		if err != nil {
			return fmt.Errorf("db lookup %s: %w", relPath, err)
		}
		if existing != nil {
			result.Skipped++
			return nil
		}

		// Record in DB
		fileID, err := database.InsertFile(&db.File{
			SessionID:      sessionID,
			Filename:       relPath,
			SizeBytes:      info.Size(),
			ChecksumSHA256: checksum,
			State:          db.FileDiscovered,
		})
		if err != nil {
			return fmt.Errorf("db insert %s: %w", relPath, err)
		}

		// Copy to upload queue
		dst := filepath.Join(cfg.UploadQueue, relPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("failed to create upload queue directories: %w", err)
		}

		copyStart := time.Now()
		if err := copyAndVerify(path, dst, checksum); err != nil {
			return fmt.Errorf("copy %s: %w", relPath, err)
		}
		ingestMs := time.Since(copyStart).Milliseconds()

		// Mark as copied + queued, and record how long the local copy took.
		database.UpdateFileState(fileID, db.FileCopied)
		database.UpdateFileState(fileID, db.FileQueued)
		_ = database.SetFileIngestMs(fileID, ingestMs)

		result.FilesCopied++
		result.BytesCopied += info.Size()

		return nil
	})

	if n := result.SkippedSystem + result.SkippedTooSmall + result.SkippedTooLarge + result.SkippedExtension; n > 0 {
		log.Printf("ingest: ignored %d files (system=%d, under-size=%d, over-size=%d, ignored-ext=%d)",
			n, result.SkippedSystem, result.SkippedTooSmall, result.SkippedTooLarge, result.SkippedExtension)
	}

	if err != nil {
		return result, provisioned, err
	}

	return result, provisioned, nil
}

// ApplyRetention frees space on the virtual drive when it exceeds
// thresholdPercent by deleting the oldest already-uploaded files. It MUST be
// called while the drive is mounted at cfg.MountPoint (i.e. mid-maintenance,
// before Unmount). It only ever removes files the DB records as uploaded, and
// stops as soon as usage drops back under the threshold. Returns the number of
// files deleted.
func ApplyRetention(cfg IngestConfig, database *db.DB, thresholdPercent int) (int, error) {
	if thresholdPercent <= 0 || thresholdPercent >= 100 {
		return 0, nil
	}

	usedPct, err := diskUsagePercent(cfg.MountPoint)
	if err != nil {
		return 0, fmt.Errorf("statfs %s: %w", cfg.MountPoint, err)
	}
	if usedPct < float64(thresholdPercent) {
		return 0, nil
	}

	files, err := database.GetUploadedFilesEligibleForDeletion()
	if err != nil {
		return 0, fmt.Errorf("list uploaded files: %w", err)
	}

	deleted := 0
	for _, f := range files {
		if usedPct < float64(thresholdPercent) {
			break
		}
		path := filepath.Join(cfg.MountPoint, f.Filename)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("retention: could not delete %s: %v", f.Filename, err)
			continue
		}
		if err := database.UpdateFileState(f.ID, db.FileDeleted); err != nil {
			log.Printf("retention: could not mark %s deleted in db: %v", f.Filename, err)
		}
		deleted++
		// Recompute after each delete; exFAT frees space immediately.
		if usedPct, err = diskUsagePercent(cfg.MountPoint); err != nil {
			break
		}
	}
	return deleted, nil
}

func diskUsagePercent(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := float64(st.Blocks)
	if total == 0 {
		return 0, nil
	}
	free := float64(st.Bfree)
	return (total - free) / total * 100.0, nil
}

func isSystemFileOrInHiddenFolder(relPath string) bool {
	parts := strings.Split(relPath, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "._") {
			return true
		}
	}
	return false
}

// isProvisionFile reports whether relPath is one of our provisioning sentinels.
// These are consumed during provisioning and must never be ingested/uploaded,
// independent of the user-configurable "skip system files" toggle.
func isProvisionFile(relPath string) bool {
	for _, part := range strings.Split(relPath, string(filepath.Separator)) {
		if part == "ivault.provision" || part == "ivault-provision.json" {
			return true
		}
	}
	return false
}

// copyAndVerify copies src to dst while simultaneously computing the SHA-256
// of the written bytes via io.TeeReader. If the resulting hash does not match
// expectedChecksum the destination file is deleted and an error is returned.
// This reduces ingest I/O from three full file reads to two (source checksum
// for dedup + this combined copy-and-verify).
func copyAndVerify(src, dst, expectedChecksum string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	hasher := sha256.New()
	if _, err := io.Copy(out, io.TeeReader(in, hasher)); err != nil {
		os.Remove(dst)
		return err
	}

	if err := out.Sync(); err != nil {
		os.Remove(dst)
		return err
	}

	got := fmt.Sprintf("%x", hasher.Sum(nil))
	if got != expectedChecksum {
		os.Remove(dst)
		return fmt.Errorf("checksum mismatch after copy (expected %s, got %s) — file deleted", expectedChecksum, got)
	}

	return nil
}
