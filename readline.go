package main

// readline.go — минимальный line-editing для CLI без внешних зависимостей:
// raw-режим терминала через ioctl, стрелки ←/→ (курсор), ↑/↓ (история), Home/End, Backspace.
// Не-TTY stdin (пайпы) → вызывающий код падает обратно на bufio.Scanner.

import (
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// cliRawMode — переводит stdin в raw (без ICANON/ECHO); возвращает restore и ok.
func cliRawMode() (func(), bool) {
	fd := int(os.Stdin.Fd())
	var old syscall.Termios
	if _, _, e := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0); e != 0 {
		return nil, false
	}
	nw := old
	nw.Lflag &^= syscall.ICANON | syscall.ECHO
	nw.Iflag &^= syscall.ICRNL | syscall.IXON
	nw.Cc[syscall.VMIN] = 1
	nw.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&nw)), 0, 0, 0); e != 0 {
		return nil, false
	}
	return func() {
		syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}, true
}

// cliReadLine — строка со стрелками и историей; ok=false на EOF/Ctrl+D/Ctrl+C.
func cliReadLine(prompt string, history *[]string) (string, bool) {
	os.Stdout.WriteString(prompt)
	buf := make([]rune, 0, 64)
	pos := 0
	hidx := len(*history)
	saved := ""
	redraw := func() {
		os.Stdout.WriteString("\r" + prompt + string(buf) + "\x1b[K")
		if d := len(buf) - pos; d > 0 {
			os.Stdout.WriteString("\x1b[" + itoa(d) + "D")
		}
	}
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if n == 0 || err != nil {
			return "", false
		}
		b := one[0]
		switch {
		case b == '\r' || b == '\n':
			os.Stdout.WriteString("\r\n")
			line := string(buf)
			if strings.TrimSpace(line) != "" {
				*history = append(*history, line)
			}
			return line, true
		case b == 3: // Ctrl+C
			os.Stdout.WriteString("^C\r\n")
			return "", false
		case b == 4: // Ctrl+D
			if len(buf) == 0 {
				return "", false
			}
		case b == 0x7f || b == 8: // Backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
				redraw()
			}
		case b == 0x1b: // escape-последовательность
			seq := make([]byte, 2)
			if _, err := io.ReadFull(os.Stdin, seq); err != nil {
				continue
			}
			if seq[0] != '[' {
				continue
			}
			switch seq[1] {
			case 'A': // ↑ история назад
				if hidx > 0 {
					if hidx == len(*history) {
						saved = string(buf)
					}
					hidx--
					buf = []rune((*history)[hidx])
					pos = len(buf)
					redraw()
				}
			case 'B': // ↓ история вперёд
				if hidx < len(*history)-1 {
					hidx++
					buf = []rune((*history)[hidx])
				} else {
					hidx = len(*history)
					buf = []rune(saved)
				}
				pos = len(buf)
				redraw()
			case 'C': // →
				if pos < len(buf) {
					pos++
					redraw()
				}
			case 'D': // ←
				if pos > 0 {
					pos--
					redraw()
				}
			case 'H': // Home
				pos = 0
				redraw()
			case 'F': // End
				pos = len(buf)
				redraw()
			}
		case b >= 0x20: // печатный символ (с UTF-8)
			rb := []byte{b}
			if b >= 0x80 {
				size := 0
				switch {
				case b&0xE0 == 0xC0:
					size = 2
				case b&0xF0 == 0xE0:
					size = 3
				case b&0xF8 == 0xF0:
					size = 4
				default:
					continue
				}
				rest := make([]byte, size-1)
				if _, err := io.ReadFull(os.Stdin, rest); err != nil {
					continue
				}
				rb = append(rb, rest...)
			}
			r, _ := utf8.DecodeRune(rb)
			if r == utf8.RuneError {
				continue
			}
			buf = append(buf, 0)
			copy(buf[pos+1:], buf[pos:])
			buf[pos] = r
			pos++
			redraw()
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [8]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
