//go:build !darwin

package main

import (
	"fmt"
	"os/exec"
	"runtime"
)

func hideZoomButton() {}

func openDirectory(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer.exe", path).Start()
	case "linux":
		return exec.Command("xdg-open", path).Start()
	default:
		return fmt.Errorf("opening the diagnostics directory is unsupported on %s", runtime.GOOS)
	}
}
