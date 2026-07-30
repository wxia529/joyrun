//go:build !windows

package localfs

import "os"

func replaceInstalledFile(source, destination string) error {
	return os.Rename(source, destination)
}
