package wltest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func Compositor(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("LABWC")
	if bin == "" {
		t.Skip("set LABWC to a labwc binary to run the compositor tests")
	}
	dir, err := os.MkdirTemp("", "wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"WLR_BACKENDS=headless", "WLR_HEADLESS_OUTPUTS=1", "WAYLAND_DISPLAY=")
	log, err := os.Create(filepath.Join(dir, "labwc.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if out, _ := os.ReadFile(log.Name()); t.Failed() {
			t.Logf("compositor log:\n%s", out)
		}
	})
	for range 100 {
		if found, _ := filepath.Glob(filepath.Join(dir, "wayland-[0-9]")); len(found) > 0 {
			return found[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := os.ReadFile(log.Name())
	t.Fatalf("the compositor did not start:\n%s", out)
	return ""
}
