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
	udcName   = "fc000000.usb"
	udcPath   = "/sys/class/udc/fc000000.usb"
)

func Attach(imagePath string) error {
	// Clean up any existing gadget
	Detach()

	// Wait for gadget dir to be gone, not for UDC state
	// UDC stays "configured" when cable is plugged in — that's normal
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

func Detach() error {
	if _, err := os.Stat(gadgetDir); os.IsNotExist(err) {
		// Gadget dir gone but UDC might still be bound
		// Write empty to UDC gadget softlink if it exists
		os.WriteFile(udcPath+"/soft_connect", []byte("0"), 0644)
		return nil
	}

	// Disable UDC via gadget dir
	writeFile(gadgetDir+"/UDC", "")
	time.Sleep(1 * time.Second)

	steps := []string{
		gadgetDir + "/configs/c.1/mass_storage.0",
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

func State() string {
	b, err := os.ReadFile(udcPath + "/state")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

func IsAttached() bool {
	_, err := os.Stat(gadgetDir)
	return err == nil
}

func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0644)
}
