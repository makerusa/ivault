package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/umarsear/ivault/internal/db"
	"github.com/umarsear/ivault/internal/gadget"
	"github.com/umarsear/ivault/internal/ingest"
	"github.com/umarsear/ivault/internal/state"
	"github.com/umarsear/ivault/internal/upload"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	database, err := db.Open("/var/lib/ivault/ivault.db")
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}
	defer database.Close()

	if err := startupRecovery(database); err != nil {
		log.Printf("startup recovery warning: %v", err)
	}

	sm := state.New()
	sm.OnChange(func(old, new state.State) {
		msg := "state transition: " + old.String() + " → " + new.String()
		log.Println(msg)
		database.Log("info", "state", msg)
	})

	monitor := gadget.NewMonitor()
	monitor.Start(ctx)

	var uploadCancel context.CancelFunc

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
					if s == state.StateUploading || s == state.StateMaintenance {
						log.Println("device plugged in during sync — interrupting")
						database.Log("warn", "gadget", "device plugged in during sync — interrupting")

						if uploadCancel != nil {
							uploadCancel()
						}

						ingest.Unmount()

						sm.Transition(state.StateAttaching)
						if err := gadget.Attach("/nvme/usb_disk.img"); err != nil {
							log.Println("reattach error:", err)
							sm.Transition(state.StateError)
						} else {
							sm.Transition(state.StateRecording)
						}
					}
				}
			}
		}
	}()

	// Attach gadget
	sm.Transition(state.StateAttaching)
	if err := gadget.Attach("/nvme/usb_disk.img"); err != nil {
		log.Fatalf("failed to attach gadget: %v", err)
	}
	sm.Transition(state.StateRecording)
	log.Println("iVault ready — gadget state:", gadget.State())
	database.Log("info", "main", "iVault started")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	log.Println("Send SIGUSR1 to trigger maintenance: kill -USR1", os.Getpid())

	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGUSR1:
				log.Println("maintenance triggered via signal")
				uploadCancel = runMaintenance(ctx, sm, database)

			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("shutdown signal received")
				database.Log("info", "main", "shutdown initiated")
				sm.Transition(state.StateShuttingDown)

				if uploadCancel != nil {
					uploadCancel()
				}

				ingest.Unmount()

				if err := gadget.Detach(); err != nil {
					log.Println("detach error on shutdown:", err)
				}

				database.Log("info", "main", "shutdown complete")
				log.Println("iVault stopped cleanly")
				os.Exit(0)
			}
		}
	}
}

func runMaintenance(ctx context.Context, sm *state.Machine, database *db.DB) context.CancelFunc {
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

		// Detach
		sm.Transition(state.StateDetaching)
		if err := gadget.Detach(); err != nil {
			log.Println("detach error:", err)
			database.EndSession(sessionID, 0, 0, 0, "error")
			sm.Transition(state.StateError)
			uploadCancel()
			return
		}
		time.Sleep(500 * time.Millisecond)

		// Mount
		sm.Transition(state.StateMaintenance)
		if err := ingest.Mount(); err != nil {
			log.Println("mount error:", err)
			database.Log("error", "ingest", err.Error())
			gadget.Attach("/nvme/usb_disk.img")
			database.EndSession(sessionID, 0, 0, 0, "error")
			sm.Transition(state.StateRecording)
			uploadCancel()
			return
		}
		log.Println("disk image mounted")

		// Ingest with full tracking
		result, err := ingest.Run(database, sessionID)
		if err != nil {
			log.Println("ingest error:", err)
			database.Log("warn", "ingest", fmt.Sprintf("ingest error: %v", err))
		}
		log.Printf("ingest: found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied)
		database.Log("info", "ingest", fmt.Sprintf(
			"found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied,
		))

		// Unmount
		if err := ingest.Unmount(); err != nil {
			log.Println("unmount error:", err)
		}

		// Reattach
		sm.Transition(state.StateAttaching)
		if err := gadget.Attach("/nvme/usb_disk.img"); err != nil {
			log.Println("reattach error:", err)
			database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "error")
			sm.Transition(state.StateError)
			uploadCancel()
			return
		}
		sm.Transition(state.StateRecording)
		log.Println("gadget reattached — device can record again")

		database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "complete")

		// Upload in background
		sm.Transition(state.StateUploading)
		go func() {
			defer sm.Transition(state.StateRecording)

			select {
			case <-uploadCtx.Done():
				log.Println("upload cancelled before start")
				return
			default:
			}

			log.Println("starting upload...")
			uploaded, err := upload.UploadAll(uploadCtx, database)
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

func startupRecovery(database *db.DB) error {
	log.Println("running startup recovery...")

	// Unmount if stuck
	ingest.Unmount()

	// Reset any files stuck in uploading state
	if err := database.ResetStuckFiles(); err != nil {
		return fmt.Errorf("reset stuck files: %w", err)
	}

	log.Println("startup recovery complete")
	return nil
}
