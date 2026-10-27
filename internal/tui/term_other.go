//go:build !linux

package tui

import (
	"io"
	"os"
)

func makeRaw(*os.File) func() { return func() {} }

func terminalSize(io.Writer) (int, int) { return 100, 30 }
