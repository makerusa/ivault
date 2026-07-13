package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
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
	"github.com/makerusa/ivault/internal/schedule"
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

// uploadDeviceFolder is the per-device folder used at the destination so
// multiple devices don't write into one shared folder. Prefers the friendly
// device name, falls back to the device id, and strips path separators so it
// stays a single folder segment.
func uploadDeviceFolder(cfg *config.Config) string {
	name := strings.TrimSpace(cfg.DeviceName)
	if name == "" {
		name = cfg.DeviceID
	}
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return strings.TrimSpace(name)
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
		Workers:      cfg.UploadWorkers,
		DeviceFolder: uploadDeviceFolder(cfg),
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

	// Automatic scheduler. The schedule is portal-defined (delivered via the
	// heartbeat, persisted in the config table) and re-read every tick so it can
	// change at runtime: fire a sync at the configured time on the selected
	// weekdays, and never during a blackout window. Manual triggers are also
	// blocked during blackout. We tick every 30s and de-dupe by minute so a
	// scheduled minute fires exactly once even if a tick lands slightly off.
	scheduleTicker := time.NewTicker(30 * time.Second)
	defer scheduleTicker.Stop()
	lastFiredMinute := ""
	log.Println("scheduler: portal-defined schedule (manual sync always available unless in a blackout window)")

	for {
		select {
		case now := <-scheduleTicker.C:
			sch := schedule.Load(database)
			if !sch.MatchesTime(now) {
				continue
			}
			minute := now.Format("2006-01-02 15:04")
			if minute == lastFiredMinute {
				continue
			}
			if sch.InBlackout(now) {
				log.Println("scheduler: scheduled time reached but inside a blackout window — skipping")
				lastFiredMinute = minute
				continue
			}
			lastFiredMinute = minute
			log.Println("maintenance triggered by scheduler")
			if fn := runMaintenance(ctx, sm, database, cfg, ingestCfg, uploadCfg, true); fn != nil {
				holder.set(fn)
			}

		case sig := <-sigs:
			switch sig {
			case syscall.SIGUSR1:
				// Manual/portal-triggered sync — still refused during a blackout.
				if schedule.Load(database).InBlackout(time.Now()) {
					log.Println("manual sync requested but a blackout window is active — refused")
					database.Log("warn", "scheduler", "manual sync refused: blackout window active")
					continue
				}
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

		// Eject the MEDIA only — clear the mass-storage backing file so the host
		// sees an empty reader, while the USB gadget stays bound to the controller.
		// We deliberately do NOT unbind the UDC here (issue #7): on the RK3576 the
		// unbind→rebind cycle races the dwc3 controller and intermittently crashes
		// the board, and the unbind is frequently a no-op anyway. The LUN is
		// removable, so a media-change (eject → reload) is enough for the host to
		// drop and re-read the volume without a USB re-enumeration.
		sm.Transition(state.StateDisconnecting)
		if reattachAfter {
			log.Println("maintenance — ejecting media (gadget stays attached)")
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
				gadget.Load(cfg.ImagePath) // re-present media; UDC stays bound
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
			// Re-present the media by re-setting the backing file. The UDC was
			// never unbound (issue #7), so there is no rebind to do — reloading
			// the backing file is what the host sees as "media inserted".
			if err := gadget.Load(cfg.ImagePath); err != nil {
				log.Println("load error:", err)
				database.EndSession(sessionID, result.FilesFound, result.FilesCopied, result.BytesCopied, "error")
				sm.Transition(state.StateError)
				uploadCancel()
				return
			}
			log.Println("media re-inserted — drive available to host again")
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
				// Record last successful sync (data reached the cloud) for the
				// portal's "Last sync" — reported via the next heartbeat.
				if len(uploaded) > 0 {
					_ = database.SetConfig("last_sync_at", time.Now().UTC().Format(time.RFC3339))
					if len(dests) > 0 {
						name := dests[0].Name
						if name == "" {
							name = dests[0].Type
						}
						_ = database.SetConfig("last_sync_destination", name)
						// Tell the portal this destination just took a real backup
						// so its status reflects reality (reachable + last backup),
						// not just whether a manual test was ever run.
						agent.ReportUploadSuccess(cfg, dests[0].ID)
					}
				}
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
