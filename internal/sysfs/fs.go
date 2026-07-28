package sysfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadDir(path string) ([]fs.DirEntry, error)
	Stat(path string) (fs.FileInfo, error)
}

type OSFileSystem struct {
	Root string
}

func (o OSFileSystem) resolve(relative string) (string, error) {
	cleaned := filepath.Clean(strings.TrimLeft(relative, `/\`))
	if cleaned == "." || cleaned == "" {
		return filepath.Clean(o.Root), nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes sysfs root")
	}
	return filepath.Join(o.Root, cleaned), nil
}

func (o OSFileSystem) ReadFile(path string) ([]byte, error) {
	resolved, err := o.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}

func (o OSFileSystem) WriteFile(path string, data []byte) error {
	resolved, err := o.resolve(path)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(resolved, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	// A sysfs store callback completes during Write. fsync is not supported by
	// every sysfs attribute and may return EINVAL after a successful write.
	return nil
}

func (o OSFileSystem) ReadDir(path string) ([]fs.DirEntry, error) {
	resolved, err := o.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(resolved)
}

func (o OSFileSystem) Stat(path string) (fs.FileInfo, error) {
	resolved, err := o.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.Stat(resolved)
}
