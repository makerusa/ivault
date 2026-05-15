package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/makerusa/ivault/internal/agent"
	"github.com/makerusa/ivault/internal/config"
	"github.com/makerusa/ivault/internal/db"
	"github.com/makerusa/ivault/internal/gadget"
	"github.com/makerusa/ivault/internal/ingest"
	"github.com/makerusa/ivault/internal/state"
	"github.com/makerusa/ivault/internal/upload"
)

// cancelHolder guards the upload cancel function against concurrent access
// from the signal-handler goroutine and the UDC event goroutine.
type cancelHolder struct {
	mu sync.Mutex
	fn context.CancelFunc
}

func (c *cancelHolder) set(fn context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fn = fn
}

func (c *cancelHolder) call() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fn != nil {
		c.fn()
	}
}

func main() {
	cfgPath := flag.String("config", "/etc/ivault/config.json", "path to JSON config file")
	flag.Parse()

	cfg, err := config.LoadOrDefault(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ingestCfg := ingest.IngestConfig{
		ImagePath:   cfg.ImagePath,
		MountPoint:  cfg.MountPoint,
		UploadQueue: cfg.UploadQueue,
		ConfigPath:  *cfgPath,
	}
	uploadCfg := upload.UploadConfig{
		UploadQueue:  cfg.UploadQueue,
		RcloneRemote: cfg.RcloneRemote,
		RclonePath:   cfg.RclonePath,
		DestID:       1,
		Workers:      cfg.UploadWorkers,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}
	defer database.Close()

	if err := startupRecovery(database, ingestCfg); err != nil {
		log.Printf("startup recovery warning: %v", err)
	}

	sm := state.New()
	sm.OnChange(func(old, new state.State) {
		msg := "state transition: " + old.String() + " → " + new.String()
		log.Println(msg)
		database.Log("info", "state", msg)
	})

	monitor := gadget.NewMonitor(cfg.UDCName)
	monitor.Start(ctx)

	var holder cancelHolder

	// UDC event handler
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-monitor.Events():
				log.Println("UDC event:", event)
				database.Log("info", "gadget", "UDC event: "+event.String())

				if event == gadget.UDCPlugged {
					s := sm.State()
					if s == state.StateUploading || s == state.StateMaintenance || s == state.StateOffline {
						if s == state.StateOffline {
							log.Println("device plugged in — loading disk image")
							database.Log("info", "gadget", "device plugged in after offline")
						} else {
							log.Println("device plugged in during sync — interrupting")
							database.Log("warn", "gadget", "device plugged in during sync — interrupting")
							holder.call()
							ingest.Unmount(ingestCfg)
						}

						sm.Transition(state.StateAttaching)
						if err := gadget.Load(cfg.ImagePath); err != nil {
							log.Println("load error:", err)
							sm.Transition(state.StateError)
						} else {
							sm.Transition(state.StateRecording)
						}
					}
				} else if event == gadget.UDCUnplugged {
					s := sm.State()
					if s == state.StateRecording {
						log.Println("device unplugged — triggering automatic maintenance")
						// reattachAfter=false: host is gone, so wait for re-plug
						if fn := runMaintenance(ctx, sm, database, cfg, ingestCfg, uploadCfg, false); fn != nil {
							holder.set(fn)
						}
					}
				}
			}
		}
	}()

	// Attach gadget
	sm.Transition(state.StateAttaching)
	if err := gadget.Attach(cfg.ImagePath, cfg.UDCName); err != nil {
		log.Fatalf("failed to attach gadget: %v", err)
	}
	sm.Transition(state.StateRecording)
	log.Println("iVault ready — gadget state:", gadget.State(cfg.UDCName))
	database.Log("info", "main", "iVault started")

	// Start Heartbeat Agent
	agent.Start(ctx, cfg, sm)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	log.Println("Send SIGUSR1 to trigger maintenance: kill -USR1", os.Getpid())

	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGUSR1:
				log.Println("maintenance triggered via signal")
				// reattachAfter=true: manually triggered while plugged in
				if fn := runMaintenance(ctx, sm, database, cfg, ingestCfg, uploadCfg, true); fn != nil {
					holder.set(fn)
				}

			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("shutdown signal received")
				database.Log("info", "main", "shutdown initiated")
				sm.Transition(state.StateShuttingDown)

				holder.call()

				ingest.Unmount(ingestCfg)

				if err := gadget.Detach(cfg.UDCName); err != nil {
					log.Println("detach error on shutdown:", err)
				}

				database.Log("info", "main", "shutdown complete")
				log.Println("iVault stopped cleanly")
				// Return instead of os.Exit so defers (database.Close) run cleanly.
				return
			}
		}
	}
}

func runMaintenance(
	ctx context.Context,
	sm *state.Machine,
	database *db.DB,
	cfg *config.Config,
	ingestCfg ingest.IngestConfig,
	uploadCfg upload.UploadConfig,
	reattachAfter bool,
) context.CancelFunc {
	s := sm.State()
	if s == state.StateMaintenance ||
		s == state.StateDetaching ||
		s == state.StateAttaching {
		log.Println("maintenance already in progress — skipping")
		return nil
	}

	uploadCtx, uploadCancel := context.WithCancel(ctx)

	go func() {
		log.Println("--- maintenance cycle starting ---")

		// Start session
		sessionID, err := database.StartSession()
		if err != nil {
			log.Println("failed to start session:", err)
		}

		// Eject (makes host see "empty drive")
		sm.Transition(state.StateDetaching)
		if err := gadget.Eject(); err != nil {
			log.Println("eject error:", err)
		}
		// Brief settle time so the USB host sees the "media removed" before we mount.
		time.Sleep(1 * time.Second)

		// Mount
		sm.Transition(state.StateMaintenance)
		if err := ingest.Mount(ingestCfg); err != nil {
			log.Println("mount error:", err)
			database.Log("error", "ingest", err.Error())
			gadget.Load(cfg.ImagePath)
			database.EndSession(sessionID, 0, 0, 0, "error")
			sm.Transition(state.StateRecording)
			uploadCancel()
			return
		}
		log.Println("disk image mounted")

		// Ingest with full tracking
		result, provisioned, err := ingest.Run(ingestCfg, database, sessionID)
		if err != nil {
			log.Println("ingest error:", err)
			database.Log("warn", "ingest", fmt.Sprintf("ingest error: %v", err))
		}
		
		if result == nil {
			result = &ingest.IngestResult{}
		}

		if provisioned {
			log.Println("device was just provisioned — reloading config and starting agent")
			newCfg, err := config.LoadOrDefault(ingestCfg.ConfigPath)
			if err == nil {
				*cfg = *newCfg
				agent.Start(ctx, cfg, sm)
			}
		}

		log.Printf("ingest: found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied)
		database.Log("info", "ingest", fmt.Sprintf(
			"found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied,
		))

		// Unmount local filesystem
		if err := ingest.Unmount(ingestCfg); err != nil {
			log.Println("unmount error:", err)
		}

		if reattachAfter {
			sm.Transition(state.StateAttaching)
			if err := gadget.Load(cfg.ImagePath); err != nil {
				log.Println("load error:", err)
				database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "error")
				sm.Transition(state.StateError)
				uploadCancel()
				return
			}
			log.Println("gadget reloaded — device can record again")
		} else {
			sm.Transition(state.StateOffline)
			log.Println("maintenance complete — waiting for host to plug back in")
		}

			database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "complete")

			// Upload in background (network-based, runs regardless of USB state)
			sm.Transition(state.StateUploading)
			go func() {
				// Return to the correct state after upload depending on whether
				// the host was still connected when maintenance ran.
				if reattachAfter {
					defer sm.Transition(state.StateRecording)
				} else {
					defer sm.Transition(state.StateOffline)
				}

				select {
				case <-uploadCtx.Done():
					log.Println("upload cancelled before start")
					return
				default:
				}

				log.Println("starting upload...")
				uploaded, err := upload.UploadAll(uploadCtx, database, uploadCfg)
				if err != nil {
					log.Println("upload error:", err)
					database.Log("error", "upload", err.Error())
					return
				}
				log.Printf("uploaded %d files", len(uploaded))
				database.Log("info", "upload", fmt.Sprintf("uploaded %d files", len(uploaded)))
				log.Println("--- maintenance cycle complete ---")
			}()
		}()

	return uploadCancel
}

func startupRecovery(database *db.DB, ingestCfg ingest.IngestConfig) error {
	log.Println("running startup recovery...")

	// Unmount if stuck from a previous crash or power loss
	ingest.Unmount(ingestCfg)

	// Reset any files stuck in uploading state back to queued for retry
	if err := database.ResetStuckFiles(); err != nil {
		return fmt.Errorf("reset stuck files: %w", err)
	}

	log.Println("startup recovery complete")
	return nil
}
