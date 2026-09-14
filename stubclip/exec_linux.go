//go:build linux

package clipboard

import (
	"io"
	"os"
	"syscall"
)

// forkExecInput: запускает bin, пишет s в stdin, возвращает pid.
func forkExecInput(bin, s string) (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	pid, err := syscall.ForkExec(bin, []string{bin}, &syscall.ProcAttr{
		Dir:   "/",
		Env:   syscall.Environ(),
		Files: []uintptr{r.Fd(), 1, 2},
	})
	r.Close()
	if err != nil {
		w.Close()
		return 0, err
	}
	go func() {
		io.WriteString(w, s)
		w.Close()
	}()
	return pid, nil
}

// forkExecOutput: запускает bin, возвращает stdout как строку.
func forkExecOutput(bin string) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	pid, err := syscall.ForkExec(bin, []string{bin}, &syscall.ProcAttr{
		Dir:   "/",
		Env:   syscall.Environ(),
		Files: []uintptr{0, w.Fd(), 2},
	})
	w.Close()
	if err != nil {
		r.Close()
		return "", err
	}
	data, _ := io.ReadAll(r)
	r.Close()
	var ws syscall.WaitStatus
	syscall.Wait4(pid, &ws, 0, nil)
	return string(data), nil
}
