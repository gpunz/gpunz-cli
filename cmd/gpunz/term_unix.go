//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// enableVT is a no-op: ANSI escapes work on Unix terminals.
func enableVT(f *os.File) bool { return true }

func systemLocale() string { return "" }

func openBrowser(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, u)
	if err := cmd.Start(); err != nil {
		if p, lerr := exec.LookPath("wslview"); lerr == nil {
			return exec.Command(p, u).Start()
		}
		return err
	}
	go cmd.Wait()
	return nil
}

// configDir is ~/.config/gpunz on Linux and macOS.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gpunz"), nil
}
