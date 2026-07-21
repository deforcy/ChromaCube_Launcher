//go:build !darwin

package main

import "os"

// commitHostsFile writes the new hosts-file contents directly. On Windows the
// app already runs elevated (forced by the exe manifest), and on Linux it is
// expected to be started with the privileges needed to edit /etc/hosts, so a
// plain write is all that is required here.
func commitHostsFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
