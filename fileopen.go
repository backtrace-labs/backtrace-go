package bt

import (
	"errors"
	"os"
)

var errNotRegularFile = errors.New("not a regular file")

type fileOpener interface {
	Stat(name string) (os.FileInfo, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
}

type hostFiles struct{}

func (hostFiles) Stat(name string) (os.FileInfo, error) { return os.Stat(name) }

func (hostFiles) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func openRegularFile(files fileOpener, name string) (*os.File, os.FileInfo, error) {
	info, err := files.Stat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errNotRegularFile
	}
	// A blocking open of a FIFO waits for a writer, and the path can change after Stat.
	f, err := files.OpenFile(name, readNonBlocking, 0)
	if err != nil {
		return nil, nil, err
	}
	// Stat the opened handle (not the path) so a file swapped
	// between stat and open cannot bypass the checks.
	if info, err = f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errNotRegularFile
		}
		return nil, nil, err
	}
	return f, info, nil
}
