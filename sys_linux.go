package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func hideConsole(*exec.Cmd) {}

func autostartFile() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "autostart", "ponte.desktop")
}

func autostartEnabled() bool {
	_, err := os.Stat(autostartFile())
	return err == nil
}

func setAutostart(on bool) error {
	p := autostartFile()
	if !on {
		return os.Remove(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	return os.WriteFile(p, []byte(fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Ponte\nExec=\"%s\" --background\nX-GNOME-Autostart-enabled=true\n", exe)), 0o644)
}

func canFixPermissions() bool {
	_, err := exec.LookPath("pkexec")
	return err == nil
}

// fixPermissions lets the logged-in user read input devices and use
// /dev/uinput, through udev "uaccess" rules. It asks for the admin password.
func fixPermissions() error {
	script := `set -e
echo uinput > /etc/modules-load.d/ponte.conf
cat > /etc/udev/rules.d/70-ponte.rules <<'EOF'
KERNEL=="uinput", SUBSYSTEM=="misc", OPTIONS+="static_node=uinput", TAG+="uaccess"
SUBSYSTEM=="input", KERNEL=="event*", TAG+="uaccess"
EOF
modprobe uinput || true
udevadm control --reload-rules
udevadm trigger --subsystem-match=input --subsystem-match=misc
udevadm settle || true
`
	out, err := exec.Command("pkexec", "sh", "-c", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func logCursor() {}
