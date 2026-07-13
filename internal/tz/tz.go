// Package tz centralizes applying the account timezone to the device: it
// persists the schedule timezone (read by the schedule package) and, when it
// changes, sets the system timezone so the device's wall-clock matches the
// user's account. Both the provisioning step and the heartbeat call it, so the
// logic lives in one place.
package tz

import (
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/makerusa/ivault/internal/db"
)

// Apply validates the IANA timezone, persists it to sched_timezone, and — only
// when it changed — best-effort sets the system timezone via timedatectl.
// Invalid zones are ignored so a bad value can't wedge provisioning or a
// heartbeat. Safe to call on every heartbeat.
func Apply(database *db.DB, tzName string) {
	tzName = strings.TrimSpace(tzName)
	if tzName == "" {
		return
	}
	if _, err := time.LoadLocation(tzName); err != nil {
		log.Printf("tz: ignoring invalid timezone %q: %v", tzName, err)
		return
	}
	if prev, _ := database.GetConfig("sched_timezone"); prev == tzName {
		return // unchanged — schedule already reads this; nothing to do
	}
	if err := database.SetConfig("sched_timezone", tzName); err != nil {
		log.Printf("tz: failed to persist timezone: %v", err)
	}
	go func() {
		if out, err := exec.Command("sudo", "timedatectl", "set-timezone", tzName).CombinedOutput(); err != nil {
			log.Printf("tz: timedatectl set-timezone %q failed: %v (%s)", tzName, err, strings.TrimSpace(string(out)))
		} else {
			log.Printf("tz: system timezone set to %s", tzName)
		}
	}()
}
