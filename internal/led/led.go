// Package led drives a board status LED (e.g. /sys/class/leds/user-led) to
// reflect the appliance state, giving a headless unit visible feedback:
// solid = ready, blinking = busy (don't unplug), rapid = error, plus a
// distinct flash when a provisioning file is applied.
//
// Blink patterns use the kernel "timer" trigger so the kernel does the
// blinking — no busy-looping goroutine.
package led

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/makerusa/ivault/internal/state"
)

type Indicator struct {
	dir     string
	maxB    string
	enabled bool
}

// New returns an indicator for /sys/class/leds/<name>. If disabled or the LED
// is absent, all operations are no-ops (safe on boards without the LED).
func New(name string, enabled bool) *Indicator {
	i := &Indicator{dir: "/sys/class/leds/" + name, maxB: "255", enabled: enabled}
	if !enabled {
		return i
	}
	if _, err := os.Stat(i.dir); err != nil {
		i.enabled = false
		return i
	}
	if b, err := os.ReadFile(i.dir + "/max_brightness"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			i.maxB = v
		}
	}
	return i
}

func (i *Indicator) write(file, val string) {
	if !i.enabled {
		return
	}
	_ = os.WriteFile(i.dir+"/"+file, []byte(val), 0o644)
}

func (i *Indicator) solid()     { i.write("trigger", "none"); i.write("brightness", i.maxB) }
func (i *Indicator) off()       { i.write("trigger", "none"); i.write("brightness", "0") }
func (i *Indicator) heartbeat() { i.write("trigger", "heartbeat") }
func (i *Indicator) blink(onMs, offMs int) {
	i.write("trigger", "timer")
	i.write("delay_on", strconv.Itoa(onMs))
	i.write("delay_off", strconv.Itoa(offMs))
}

// ForState maps a device state to an LED pattern.
func (i *Indicator) ForState(s state.State) {
	switch s {
	case state.StateConnected:
		i.solid() // ready, host present
	case state.StateDisconnected:
		i.heartbeat() // alive, idle, waiting for a host
	case state.StateError:
		i.blink(80, 80) // rapid = error
	case state.StateBooting:
		i.blink(500, 500)
	default: // connecting/disconnecting/snapshotting/syncing/archiving
		i.blink(150, 150) // busy — do not unplug
	}
}

// FlashProvisioned signals that a provisioning file was detected and applied:
// three deliberate blinks distinct from the busy pattern. Blocks ~1s; the
// caller should re-apply ForState afterwards (the next state change also will).
func (i *Indicator) FlashProvisioned() {
	if !i.enabled {
		return
	}
	i.write("trigger", "none")
	for n := 0; n < 3; n++ {
		i.write("brightness", i.maxB)
		time.Sleep(180 * time.Millisecond)
		i.write("brightness", "0")
		time.Sleep(180 * time.Millisecond)
	}
}
