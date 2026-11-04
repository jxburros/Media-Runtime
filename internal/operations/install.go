package operations

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstallEngine extracts only the contracted executable from an already verified archive.
// No archive member can create a symlink, directory traversal, or overwrite another file.
func InstallEngine(archive, directory string) error {
	source, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer source.Close()
	zipped, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer zipped.Close()
	reader := tar.NewReader(zipped)
	var executable []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag != tar.TypeReg || !safeRelative(name) {
			return errors.New("archive contains an unsafe member")
		}
		if name != "media-engine-native" {
			continue
		}
		if executable != nil || header.Size < 1 || header.Size > 256*1024*1024 {
			return errors.New("invalid or duplicate engine executable")
		}
		executable, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil {
			return err
		}
		if int64(len(executable)) != header.Size {
			return errors.New("truncated engine executable")
		}
	}
	if len(executable) < 4 || string(executable[:4]) != "\x7fELF" {
		return errors.New("archive has no Linux ELF engine executable")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	target := filepath.Join(directory, "media-engine-native")
	if _, err = os.Lstat(target); err == nil {
		return errors.New("engine target exists; install each version into its own directory")
	}
	temp, err := os.CreateTemp(directory, ".engine-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(executable); err == nil {
		err = temp.Chmod(0700)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), target)
}
