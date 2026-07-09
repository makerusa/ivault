// Package led drives a board status LED (e.g. /sys/class/leds/user-led) to show
// the appliance's provisioning/connection status on a headless unit:
//
//	slow pulse  — not provisioned, or not connected to the portal
//	rapid pulse — provisioning in progress
//	solid       — provisioned and connected to the portal
//
// Blink patterns use the kernel "timer" trigger so the kernel does the
// blinking (no busy-looping goroutine). On a board with only one usable LED
// this single indicator is time-shared; the design is per-device by nature.
package led

import (
	"os"
	"strconv"
	"strings"
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

func (i *Indicator) blink(onMs, offMs int) {
	i.write("trigger", "timer")
	i.write("delay_on", strconv.Itoa(onMs))
	i.write("delay_off", strconv.Itoa(offMs))
}

// Solid = provisioned and connected.
func (i *Indicator) Solid() { i.write("trigger", "none"); i.write("brightness", i.maxB) }

// Off turns the LED off.
func (i *Indicator) Off() { i.write("trigger", "none"); i.write("brightness", "0") }

// SlowPulse (~5s period, brief blip) = not provisioned / not connected.
func (i *Indicator) SlowPulse() { i.blink(200, 4800) }

// RapidPulse (~400ms period) = provisioning in progress.
func (i *Indicator) RapidPulse() { i.blink(200, 200) }
