//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
	procSetConsoleTitleW   = kernel32.NewProc("SetConsoleTitleW")
	procGetConsoleMode     = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode     = kernel32.NewProc("SetConsoleMode")
)

const (
	enableQuickEditMode = 0x0040
	enableExtendedFlags = 0x0080
)

func platformInit(title string) {
	// Türkçe karakterler konsolda düzgün görünsün.
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
	if p, err := syscall.UTF16PtrFromString(title); err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
	// Konsol penceresine tıklanınca "Hızlı Düzenleme" modu çıktıyı ve dolayısıyla
	// aktarımları dondurabiliyor; bu modu kapatıyoruz.
	if h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE); err == nil {
		var mode uint32
		if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r != 0 {
			mode &^= enableQuickEditMode
			mode |= enableExtendedFlags
			procSetConsoleMode.Call(uintptr(h), uintptr(mode))
		}
	}
}

func openBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func revealInExplorer(dir, file string) error {
	cmd := exec.Command("explorer.exe")
	line := `explorer.exe "` + dir + `"`
	if file != "" {
		line = `explorer.exe /select,"` + file + `"`
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func isDiskFull(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 112 || errno == 39 // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	return false
}

func waitBeforeExit() {
	fmt.Print("\nKapatmak için Enter'a basın...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
