// Package clipboard — stub for Android/Termux builds where faccessat2 is blocked by seccomp.
// Real clipboard integration is done via termux-clipboard-get/set when available.
package clipboard

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// ErrUnsupported is returned when no clipboard helper is available.
var ErrUnsupported = errors.New("clipboard: unsupported on this platform")

func lookPathSafe(name string) (string, error) {
	for _, dir := range strings.Split(getenv("PATH"), ":") {
		if dir == "" {
			continue
		}
		full := dir + "/" + name
		if statOK(full) {
			return full, nil
		}
	}
	for _, dir := range []string{"/data/data/com.termux/files/usr/bin", "/system/bin"} {
		full := dir + "/" + name
		if statOK(full) {
			return full, nil
		}
	}
	return "", ErrUnsupported
}

func getenv(k string) string {
	for _, e := range environ() {
		if strings.HasPrefix(e, k+"=") {
			return e[len(k)+1:]
		}
	}
	return ""
}

func environ() []string { return environImpl() }

// WriteAll copies text to the system clipboard (termux-clipboard-set when present).
// Запуск — сырые fork/execve/wait4: os/exec в Go 1.24 зовёт pidfd_open,
// который seccomp 32-битного Android убивает сигналом SIGSYS.
func WriteAll(text string) error {
	p, err := lookPathSafe("termux-clipboard-set")
	if err != nil {
		return ErrUnsupported
	}
	pid, err := forkExecInput(p, text)
	if err != nil {
		return err
	}
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	return err
}

// ReadAll reads the system clipboard (termux-clipboard-get when present).
func ReadAll() (string, error) {
	p, err := lookPathSafe("termux-clipboard-get")
	if err != nil {
		return "", ErrUnsupported
	}
	return forkExecOutput(p)
}
