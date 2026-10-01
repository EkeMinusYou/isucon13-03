package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncPostData(file *os.File) error {
	for {
		err := unix.Fdatasync(int(file.Fd()))
		if err != unix.EINTR {
			return err
		}
	}
}
