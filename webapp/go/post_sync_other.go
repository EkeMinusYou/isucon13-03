//go:build !linux

package main

import "os"

// Local tests also run on platforms without Linux fdatasync.
func syncPostData(file *os.File) error { return file.Sync() }
