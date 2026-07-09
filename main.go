package main

import (
	"context"
	"encoding/json"
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
	"github.com/makerusa/ivault/internal/led"
	"github.com/makerusa/ivault/internal/provision"
	"github.com/makerusa/ivault/internal/state"
	"github.com/makerusa/ivault/internal/upload"
)

// statusLED reflects device state on a board LED (headless feedback).
var statusLED *led.Indicator

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
		UploadQueue: cfg.UploadQueue,
		Workers:     cfg.UploadWorkers,
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

	statusLED = led.New(cfg.LEDName, cfg.LEDEnabled)
	statusLED.SlowPulse()         // until a heartbeat confirms we're connected
	agent.SetStatusLED(statusLED) // agent drives solid/slow-pulse from heartbeats

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
					if s == state.StateSyncing || s == state.StateSnapshotting || s == state.StateDisconnected || s == state.StateError {
						if s == state.StateDisconnected || s == state.StateError {
							log.Println("device plugged in — loading disk image")
							database.Log("info", "gadget", "device plugged in after disconnect/error")
						} else {
							log.Println("device plugged in during sync — interrupting")
							database.Log("warn", "gadget", "device plugged in during sync — interrupting")
							holder.call()
							ingest.Unmount(ingestCfg)
						}

						sm.Transition(state.StateConnecting)
						var err error
						if !gadget.IsAttached() {
							log.Println("device plugged in — performing first-time gadget attach")
							err = gadget.Attach(cfg.ImagePath, cfg.UDCName)
						} else {
							log.Println("device plugged in — loading disk image")
							err = gadget.Load(cfg.ImagePath)
						}

						if err != nil {
							log.Println("attach/load error:", err)
							sm.Transition(state.StateError)
						} else {
							sm.Transition(state.StateConnected)
						}
					}
				} else if event == gadget.UDCUnplugged {
					s := sm.State()
					if s == state.StateConnected {
						log.Println("device unplugged — triggering automatic sync")
						// reattachAfter=false: host is gone, so wait for re-plug
						if fn := runMaintenance(ctx, sm, database, cfg, ingestCfg, uploadCfg, false); fn != nil {
							holder.set(fn)
						}
					} else if s == state.StateConnecting {
						log.Println("device unplugged while connecting — transitioning to disconnected")
						sm.Transition(state.StateDisconnected)
					}
				}
			}
		}
	}()

	// Initial attach attempt
	sm.Transition(state.StateConnecting)
	if err := gadget.Attach(cfg.ImagePath, cfg.UDCName); err != nil {
		log.Printf("initial attach skipped (unplugged or busy): %v", err)
		sm.Transition(state.StateDisconnected)
	} else {
		sm.Transition(state.StateConnected)
		log.Println("Relay ready — gadget state:", gadget.State(cfg.UDCName))
	}
	database.Log("info", "main", "Relay started")

	// Start background network discovery
	agent.GlobalDiscovery.Start(ctx)

	// Start Heartbeat Agent and Log Collection
	agent.InitLogs(ctx, cfg)
	agent.Start(ctx, cfg, sm, database)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	log.Println("Send SIGUSR1 to trigger maintenance: kill -USR1", os.Getpid())

	// Automatic scheduler. "daily" ticks often but only acts inside the allowed
	// window, once per day (so disconnects never interrupt a recording session);
	// "interval" ticks at the configured interval, any time; "off" disables it.
	// A nil channel simply never selects.
	var scheduleC <-chan time.Time
	lastDailyRun := ""
	switch cfg.ScheduleMode {
	case "interval":
		if cfg.ScheduleIntervalMinutes > 0 {
			t := time.NewTicker(time.Duration(cfg.ScheduleIntervalMinutes) * time.Minute)
			defer t.Stop()
			scheduleC = t.C
			log.Printf("scheduler: interval mode — every %d minute(s)", cfg.ScheduleIntervalMinutes)
		}
	case "off":
		log.Println("scheduler: off — manual sync only (SIGUSR1/portal)")
	default: // "daily"
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		scheduleC = t.C
		log.Printf("scheduler: daily mode — one backup between %02d:00 and %02d:00",
			cfg.ScheduleWindowStartHour, cfg.ScheduleWindowEndHour)
	}

	for {
		select {
		case now := <-scheduleC:
			// In daily mode, only fire inside the allowed window and at most
			// once per calendar day. Interval mode fires on every tick.
			if cfg.ScheduleMode != "interval" {
				if !inScheduleWindow(now, cfg.ScheduleWindowStartHour, cfg.ScheduleWindowEndHour) {
					continue
				}
				day := now.Format("2006-01-02")
				if day == lastDailyRun {
					continue
				}
				lastDailyRun = day
			}
			log.Println("maintenance triggered by scheduler")
			if fn := runMaintenance(ctx, sm, database, cfg, ingestCfg, uploadCfg, true); fn != nil {
				holder.set(fn)
			}

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
				// No specific state transition needed for internal shutdown

				holder.call()

				ingest.Unmount(ingestCfg)

				if err := gadget.Detach(cfg.UDCName); err != nil {
					log.Println("detach error on shutdown:", err)
				}

				database.Log("info", "main", "shutdown complete")
				log.Println("Relay stopped cleanly")
				// Return instead of os.Exit so defers (database.Close) run cleanly.
				return
			}
		}
	}
}

// inScheduleWindow reports whether t's hour falls within [start, end).
// Handles windows that wrap past midnight (start > end, e.g. 22..5).
func inScheduleWindow(t time.Time, start, end int) bool {
	h := t.Hour()
	if start == end {
		return true // full day
	}
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end
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
	if s == state.StateSyncing ||
		s == state.StateSnapshotting ||
		s == state.StateArchiving ||
		s == state.StateDisconnecting ||
		s == state.StateConnecting {
		log.Println("sync already in progress — skipping")
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
		sm.Transition(state.StateDisconnecting)
		if reattachAfter {
			log.Println("manually triggered maintenance — soft-disconnecting UDC")
			if err := gadget.SetUDC(""); err != nil {
				log.Println("soft-disconnect UDC error:", err)
			}
			time.Sleep(500 * time.Millisecond) // brief UDC settle time
		}
		if err := gadget.Eject(); err != nil {
			log.Println("eject error:", err)
		}
		// Brief settle time so the USB host sees the "media removed" before we mount.
		time.Sleep(1 * time.Second)

		// Mount
		sm.Transition(state.StateSnapshotting)
		if err := ingest.Mount(ingestCfg); err != nil {
			log.Println("mount error:", err)
			database.Log("error", "ingest", err.Error())
			if reattachAfter {
				gadget.Load(cfg.ImagePath)
				gadget.SetUDC(cfg.UDCName)
			}
			database.EndSession(sessionID, 0, 0, 0, "error")
			if reattachAfter {
				sm.Transition(state.StateConnected)
			} else {
				sm.Transition(state.StateDisconnected)
			}
			uploadCancel()
			return
		}
		log.Println("disk image mounted")

		// If a provision file is present, show provisioning-in-progress now
		// (before the potentially slow bootstrap/network steps run).
		provisioning := provision.Detect(ingestCfg.MountPoint)
		if provisioning {
			log.Println("provision file detected — provisioning in progress")
			statusLED.RapidPulse()
		}

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
			// Leave the rapid pulse on; the agent's first heartbeat flips it to
			// solid once it confirms the portal connection.
			newCfg, err := config.LoadOrDefault(ingestCfg.ConfigPath)
			if err == nil {
				*cfg = *newCfg
				agent.Start(ctx, cfg, sm, database)
			}
		} else if provisioning {
			// A provision file was present but provisioning didn't complete →
			// return to "not connected".
			statusLED.SlowPulse()
		}

		log.Printf("ingest: found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied)
		database.Log("info", "ingest", fmt.Sprintf(
			"found=%d copied=%d skipped=%d bytes=%d",
			result.FilesFound, result.FilesCopied, result.Skipped, result.BytesCopied,
		))

		// Space-based retention while the drive is still mounted: free room by
		// deleting the oldest already-uploaded files if over threshold.
		if cfg.RetentionEnabled {
			if n, err := ingest.ApplyRetention(ingestCfg, database, cfg.RetentionThresholdPercent); err != nil {
				log.Println("retention error:", err)
			} else if n > 0 {
				log.Printf("retention: deleted %d uploaded file(s) to free space", n)
				database.Log("info", "retention", fmt.Sprintf("deleted %d uploaded files", n))
			}
		}

		// Unmount local filesystem
		if err := ingest.Unmount(ingestCfg); err != nil {
			log.Println("unmount error:", err)
		}

		if reattachAfter {
			sm.Transition(state.StateConnecting)
			if err := gadget.Load(cfg.ImagePath); err != nil {
				log.Println("load error:", err)
				database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "error")
				sm.Transition(state.StateError)
				uploadCancel()
				return
			}
			log.Println("re-enabling UDC to soft-connect")
			if err := gadget.SetUDC(cfg.UDCName); err != nil {
				log.Println("enable UDC error:", err)
			}
			log.Println("gadget reloaded — device connected again")
		} else {
			sm.Transition(state.StateDisconnected)
			log.Println("sync complete — waiting for host to plug back in")
		}

		database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "complete")

		// Check if we actually have anything to upload (new files OR existing queue)
		queueSize := 0
		if files, err := os.ReadDir(cfg.UploadQueue); err == nil {
			queueSize = len(files)
		}

		if result.FilesCopied > 0 || queueSize > 0 {
			// Upload in background (network-based, runs regardless of USB state)
			sm.Transition(state.StateArchiving)
			go func() {
				// Return to the correct state after upload depending on whether
				// the host was still connected when maintenance ran.
				if reattachAfter {
					defer sm.Transition(state.StateConnected)
				} else {
					defer sm.Transition(state.StateDisconnected)
				}

				select {
				case <-uploadCtx.Done():
					log.Println("upload cancelled before start")
					return
				default:
				}

				log.Println("starting upload...")
				
				// Fetch latest destinations from agent memory
				rawDests := agent.GetActiveDestinations()
				log.Printf("agent: fetching latest active destinations from agent memory to start upload: count=%d", len(rawDests))
				var dests []upload.Destination
				for i, raw := range rawDests {
					var d upload.Destination
					if err := json.Unmarshal(raw, &d); err == nil {
						d.LogDetails(fmt.Sprintf("agent: [upload-prep] Destination #%d", i+1))
						dests = append(dests, d)
					} else {
						log.Printf("agent: [upload-prep] failed to parse destination #%d: %v", i+1, err)
					}
				}
				uploadCfg.Destinations = dests

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
		} else {
			log.Println("nothing to upload — skipping sync state")
			if reattachAfter {
				sm.Transition(state.StateConnected)
			} else {
				sm.Transition(state.StateDisconnected)
			}
			uploadCancel()
		}
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
