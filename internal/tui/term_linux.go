package tui

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

func ioctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// makeRaw puts a terminal into raw mode (no echo, no line buffering) and returns the restore.
func makeRaw(f *os.File) func() {
	var original syscall.Termios
	if ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&original)) != nil {
		return func() {}
	}
	raw := original
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if ioctl(f.Fd(), syscall.TCSETS, unsafe.Pointer(&raw)) != nil {
		return func() {}
	}
	return func() { _ = ioctl(f.Fd(), syscall.TCSETS, unsafe.Pointer(&original)) }
}

// terminalSize is the output terminal's columns and rows, defaulting to 100×30.
func terminalSize(w io.Writer) (int, int) {
	f, ok := w.(*os.File)
	if !ok {
		return 100, 30
	}
	var size struct{ Row, Col, X, Y uint16 }
	if ioctl(f.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&size)) != nil || size.Col == 0 || size.Row == 0 {
		return 100, 30
	}
	return int(size.Col), int(size.Row)
}
