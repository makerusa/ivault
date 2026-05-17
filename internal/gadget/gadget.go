package gadget

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	gadgetDir = "/sys/kernel/config/usb_gadget/ivault"
	udcPath   = "/sys/class/udc/"
)

// Attach creates and enables the USB mass storage gadget using the given disk
// image and UDC (USB Device Controller) name.
// The udcName can be found by running: ls /sys/class/udc/
func Attach(imagePath, udcName string) error {
	// Clean up any existing gadget
	Detach(udcName)

	// Wait for gadget dir to be gone.
	// The UDC stays "configured" when a cable is plugged in — that's normal.
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(gadgetDir); os.IsNotExist(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Small settle time for configfs
	time.Sleep(300 * time.Millisecond)

	if err := os.MkdirAll(gadgetDir, 0755); err != nil {
		return fmt.Errorf("create gadget dir: %w", err)
	}

	writes := map[string]string{
		"idVendor":  "0x1d6b",
		"idProduct": "0x0104",
		"bcdDevice": "0x0100",
		"bcdUSB":    "0x0200",
	}
	for k, v := range writes {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	if err := os.MkdirAll(gadgetDir+"/strings/0x409", 0755); err != nil {
		return err
	}
	stringWrites := map[string]string{
		"strings/0x409/serialnumber": "ivault-001",
		"strings/0x409/manufacturer": "iVault",
		"strings/0x409/product":      "iVault Storage",
	}
	for k, v := range stringWrites {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	if err := os.MkdirAll(gadgetDir+"/configs/c.1/strings/0x409", 0755); err != nil {
		return err
	}
	if err := writeFile(gadgetDir+"/configs/c.1/strings/0x409/configuration", "Mass Storage"); err != nil {
		return err
	}
	if err := writeFile(gadgetDir+"/configs/c.1/MaxPower", "250"); err != nil {
		return err
	}

	if err := os.MkdirAll(gadgetDir+"/functions/mass_storage.0", 0755); err != nil {
		return err
	}
	funcWrites := map[string]string{
		"functions/mass_storage.0/stall":           "0",
		"functions/mass_storage.0/lun.0/removable": "1",
		"functions/mass_storage.0/lun.0/ro":        "0",
		"functions/mass_storage.0/lun.0/cdrom":     "0",
		"functions/mass_storage.0/lun.0/file":      imagePath,
	}
	for k, v := range funcWrites {
		if err := writeFile(gadgetDir+"/"+k, v); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
	}

	symlink := gadgetDir + "/configs/c.1/mass_storage.0"
	if _, err := os.Lstat(symlink); os.IsNotExist(err) {
		if err := os.Symlink(gadgetDir+"/functions/mass_storage.0", symlink); err != nil {
			return fmt.Errorf("symlink: %w", err)
		}
	}

	if err := writeFile(gadgetDir+"/UDC", udcName); err != nil {
		return fmt.Errorf("enable udc: %w", err)
	}

	return nil
}

// Detach disables and tears down the USB gadget configfs tree.
// udcName is needed to issue a final soft-disconnect fallback via the UDC sysfs
// in case the gadget directory was already gone.
func Detach(udcName string) error {
	if _, err := os.Stat(gadgetDir); os.IsNotExist(err) {
		return nil
	}

	// Disable the UDC. Retry a few times since the kernel may briefly refuse
	// the write if the USB bus is mid-transaction.
	for i := 0; i < 5; i++ {
		if err := writeFile(gadgetDir+"/UDC", ""); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)

	// Teardown order matters: symlinks before directories, children before parents.
	steps := []string{
		gadgetDir + "/configs/c.1/mass_storage.0", // symlink — must go first
		gadgetDir + "/configs/c.1/strings/0x409",
		gadgetDir + "/configs/c.1",
		gadgetDir + "/functions/mass_storage.0",
		gadgetDir + "/strings/0x409",
		gadgetDir,
	}

	for _, path := range steps {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			os.Remove(path)
		} else {
			for i := 0; i < 5; i++ {
				if err := exec.Command("rmdir", path).Run(); err == nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	}

	return nil
}

// Eject removes the backing file from the mass storage gadget, making the
// host see an "empty drive". This frees the file for local mounting.
func Eject() error {
	path := gadgetDir + "/functions/mass_storage.0/lun.0/file"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	// Force a kernel sync of the backing file just in case
	exec.Command("sync").Run()

	// Try forced_eject first if it exists (bypasses host locks/prevent_medium_removal)
	forcedPath := gadgetDir + "/functions/mass_storage.0/lun.0/forced_eject"
	if _, err := os.Stat(forcedPath); err == nil {
		if errWrite := writeFile(forcedPath, "1\n"); errWrite == nil {
			time.Sleep(200 * time.Millisecond) // brief settle time for ConfigFS
			return nil
		}
	}

	// Fallback to standard eject write loop
	var err error
	for i := 0; i < 5; i++ {
		err = writeFile(path, "\n")
		if err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// Load binds the backing file to the mass storage gadget, making it visible
// to the host.
func Load(imagePath string) error {
	path := gadgetDir + "/functions/mass_storage.0/lun.0/file"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("gadget not attached")
	}

	var err error
	for i := 0; i < 5; i++ {
		err = writeFile(path, imagePath+"\n")
		if err == nil {
			return nil
		}
		// If the error is "busy", it might be because it's already loaded or 
		// the host is actively communicating. Check if the path is already correct.
		if strings.Contains(err.Error(), "device or resource busy") {
			current, _ := os.ReadFile(path)
			if strings.TrimSpace(string(current)) == strings.TrimSpace(imagePath) {
				return nil // Already loaded, ignore error
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// State returns the current UDC state string (e.g. "configured", "not attached").
func State(udcName string) string {
	b, err := os.ReadFile(udcPath + udcName + "/state")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// IsAttached reports whether the gadget configfs directory exists.
func IsAttached() bool {
	_, err := os.Stat(gadgetDir)
	return err == nil
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}
