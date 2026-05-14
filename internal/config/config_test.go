package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/makerusa/ivault/internal/config"
)

func TestDefault(t *testing.T) {
	cfg := config.Default()

	if cfg.DBPath == "" {
		t.Error("DBPath should not be empty")
	}
	if cfg.UDCName == "" {
		t.Error("UDCName should not be empty")
	}
	if cfg.UploadWorkers <= 0 {
		t.Errorf("UploadWorkers should be positive, got %d", cfg.UploadWorkers)
	}
}

func TestLoadOrDefault_NoFile(t *testing.T) {
	cfg, err := config.LoadOrDefault("/nonexistent/path/config.json")
	if err != nil {
		t.Fatalf("LoadOrDefault with missing file should not error, got: %v", err)
	}
	def := config.Default()
	if cfg.DBPath != def.DBPath {
		t.Errorf("expected default DBPath %q, got %q", def.DBPath, cfg.DBPath)
	}
}

func TestLoad_PartialOverride(t *testing.T) {
	// Write a partial config that only overrides UDCName and UploadWorkers
	partial := map[string]interface{}{
		"udc_name":       "ff400000.usb",
		"upload_workers": 4,
	}
	tmp := filepath.Join(t.TempDir(), "config.json")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(f).Encode(partial)
	f.Close()

	cfg, err := config.Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.UDCName != "ff400000.usb" {
		t.Errorf("expected UDCName %q, got %q", "ff400000.usb", cfg.UDCName)
	}
	if cfg.UploadWorkers != 4 {
		t.Errorf("expected UploadWorkers 4, got %d", cfg.UploadWorkers)
	}
	// Unspecified fields should retain defaults
	def := config.Default()
	if cfg.DBPath != def.DBPath {
		t.Errorf("unspecified DBPath should be default %q, got %q", def.DBPath, cfg.DBPath)
	}
	if cfg.RcloneRemote != def.RcloneRemote {
		t.Errorf("unspecified RcloneRemote should be default %q, got %q", def.RcloneRemote, cfg.RcloneRemote)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(tmp, []byte("{not valid json"), 0644)

	_, err := config.Load(tmp)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}
