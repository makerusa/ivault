package config

import (
	"encoding/json"
	"os"
)

// Config holds all tunable parameters for iVault. Values are loaded from a
// JSON file and fall back to sensible defaults for the reference Rock 5T
// hardware when fields are absent.
type Config struct {
	// Database
	DBPath string `json:"db_path"` // default: /var/lib/ivault/ivault.db

	// Storage layout
	ImagePath   string `json:"image_path"`   // default: /nvme/usb_disk.img
	MountPoint  string `json:"mount_point"`  // default: /nvme/ingest
	UploadQueue string `json:"upload_queue"` // default: /nvme/upload_queue

	// USB gadget
	UDCName string `json:"udc_name"` // default: fc000000.usb  (RK3588 OTG port)

	// Upload destination (rclone)
	RcloneRemote  string `json:"rclone_remote"`  // default: gdrive
	RclonePath    string `json:"rclone_path"`    // default: iVault
	UploadWorkers int    `json:"upload_workers"` // default: 2
}

// Default returns a Config populated with the reference Rock 5T defaults.
func Default() *Config {
	return &Config{
		DBPath:        "/var/lib/ivault/ivault.db",
		ImagePath:     "/nvme/usb_disk.img",
		MountPoint:    "/nvme/ingest",
		UploadQueue:   "/nvme/upload_queue",
		UDCName:       "fc000000.usb",
		RcloneRemote:  "gdrive",
		RclonePath:    "iVault",
		UploadWorkers: 2,
	}
}

// Load reads a JSON config file from path. Fields absent in the file keep
// their default values, so a partial config is valid.
func Load(path string) (*Config, error) {
	cfg := Default()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// LoadOrDefault attempts to load the config file. If the file does not exist
// it silently returns defaults; any other error is returned.
func LoadOrDefault(path string) (*Config, error) {
	cfg, err := Load(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	return cfg, err
}
