//go:build linux || android

package main

// rawproc — собственный запуск процессов без os/exec.
// Зачем: Go 1.24 в os/exec вызывает pidfd_open (syscall 434 на ARM), который
// seccomp-фильтр 32-битного Android-ядра убивает сигналом SIGSYS (не возвращает
// ENOSYS — процесс просто умирает). Поэтому все запуски внешних программ идут
// через syscall.ForkExec + Wait4 напрямую: fork+execve+wait4 — древние вызовы,
// seccomp их не трогает.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// rawProc — запущенный процесс с пайпами.
type rawProc struct {
	Pid    int
	Stdin  *os.File // писать сюда — попадёт в stdin процесса
	Stdout *os.File // читать отсюда — stdout процесса
	Stderr *os.File // читать отсюда — stderr процесса
	done   chan error
	once   sync.Once
}

// rawProcSpec — параметры запуска.
type rawProcSpec struct {
	Dir         string   // рабочий каталог (пусто = текущий)
	Env         []string // nil = наследовать os.Environ()
	InheritIO   bool     // true: процесс пишет в наш stdout/stderr (без пайпов)
	StdinData   string   // если не пусто — записать в stdin и закрыть
	NoStdin     bool     // не создавать stdin-пайп
	MergeStderr bool     // stderr → в stdout-пайп
}

// rawStart — fork+execve без pidfd_open. bin должен быть абсолютным (LookPathSafe).
func rawStart(bin string, args []string, spec rawProcSpec) (*rawProc, error) {
	if bin == "" || !strings.HasPrefix(bin, "/") {
		return nil, fmt.Errorf("rawStart: нужен абсолютный путь, получен %q", bin)
	}
	p := &rawProc{done: make(chan error, 1)}

	files := []uintptr{0, 1, 2} // fd в ребёнке: 0,1,2
	var childFiles []*os.File

	// stdin
	if spec.StdinData != "" {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		files[0] = r.Fd()
		childFiles = append(childFiles, r)
		go func() { // пишем после форка, не блокируя
			io.WriteString(w, spec.StdinData)
			w.Close()
		}()
	} else if !spec.NoStdin {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		files[0] = r.Fd()
		childFiles = append(childFiles, r)
		p.Stdin = w
	}

	// stdout
	if !spec.InheritIO {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		files[1] = w.Fd()
		childFiles = append(childFiles, w)
		p.Stdout = r
		if spec.MergeStderr {
			files[2] = w.Fd()
		} else {
			r2, w2, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			files[2] = w2.Fd()
			childFiles = append(childFiles, w2)
			p.Stderr = r2
		}
	}

	env := spec.Env
	if env == nil {
		env = syscall.Environ()
	}
	dir := spec.Dir
	if dir == "" {
		dir = "/"
	}

	pid, err := syscall.ForkExec(bin, append([]string{bin}, args...), &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: files,
	})
	// родитель закрывает детские концы пайпов
	for _, f := range childFiles {
		f.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("forkexec %s: %w", bin, err)
	}
	p.Pid = pid

	go func() {
		var ws syscall.WaitStatus
		_, werr := syscall.Wait4(pid, &ws, 0, nil)
		var err error
		switch {
		case werr != nil:
			err = werr
		case ws.Signaled():
			err = fmt.Errorf("убит сигналом %v", ws.Signal())
		case ws.ExitStatus() != 0:
			err = fmt.Errorf("exit code %d", ws.ExitStatus())
		}
		p.done <- err
		close(p.done)
	}()
	return p, nil
}

// Wait ждёт завершения и возвращает exit-ошибку (nil = код 0).
func (p *rawProc) Wait() error { return <-p.done }

// Kill мягко убивает процесс (SIGKILL; на ARM Android kill — разрешённый вызов).
func (p *rawProc) Kill() {
	if p.Pid > 0 {
		syscall.Kill(p.Pid, syscall.SIGKILL)
	}
}

// Close закрывает родительские концы пайпов.
func (p *rawProc) Close() {
	p.once.Do(func() {
		if p.Stdin != nil {
			p.Stdin.Close()
		}
		if p.Stdout != nil {
			p.Stdout.Close()
		}
		if p.Stderr != nil {
			p.Stderr.Close()
		}
	})
}

// waitCtx ждёт завершения с контекстом/таймаутом; при истечении — Kill.
func (p *rawProc) waitCtx(ctx context.Context) error {
	select {
	case err := <-p.done:
		return err
	case <-ctx.Done():
		p.Kill()
		<-p.done
		return ctx.Err()
	}
}

// runRawCombined — запустить и вернуть объединённый вывод (stdout+stderr) с таймаутом.
// Замена exec.CommandContext(...).CombinedOutput().
func runRawCombined(ctx context.Context, dir string, env []string, timeout time.Duration, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := rawStart(bin, args, rawProcSpec{Dir: dir, Env: env, NoStdin: true, MergeStderr: true})
	if err != nil {
		return "", err
	}
	outCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(p.Stdout)
		outCh <- data
	}()
	werr := p.waitCtx(cctx)
	p.Close()
	data := <-outCh
	return string(data), werr
}

// runRawOutput — только stdout.
func runRawOutput(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := rawStart(bin, args, rawProcSpec{NoStdin: true})
	if err != nil {
		return "", err
	}
	outCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(p.Stdout)
		outCh <- data
	}()
	go io.Copy(io.Discard, p.Stderr)
	werr := p.waitCtx(cctx)
	p.Close()
	data := <-outCh
	return string(data), werr
}

// runRawInput — записать stdinData в stdin, вернуть ошибку выполнения (вывод наследуется).
func runRawInput(ctx context.Context, timeout time.Duration, input string, bin string, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := rawStart(bin, args, rawProcSpec{StdinData: input, InheritIO: true})
	if err != nil {
		return err
	}
	return p.waitCtx(cctx)
}

// runRawDetached — запустить в фоне без ожидания (уведомления и т.п.).
func runRawDetached(bin string, args ...string) error {
	p, err := rawStart(bin, args, rawProcSpec{NoStdin: true, InheritIO: false})
	if err != nil {
		return err
	}
	p.Close()
	go p.Wait() // реапим зомби
	return nil
}

// shPath — абсолютный путь к sh (Termux → /system/bin/sh → /bin/sh).
func shPath() string {
	if p, err := LookPathSafe("sh"); err == nil {
		return p
	}
	for _, p := range []string{"/system/bin/sh", "/bin/sh"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "/system/bin/sh"
}

var errNoShell = errors.New("shell не найден")
