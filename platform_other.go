//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
	"syscall"
)

func platformInit(title string) {}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func revealInExplorer(dir, file string) error {
	if file != "" && runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", file).Start()
	}
	return openBrowser(dir)
}

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }

func waitBeforeExit() {}
