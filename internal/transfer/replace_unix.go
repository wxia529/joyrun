//go:build !windows

package transfer

import "os"

func replaceLocalFile(source, destination string) error {
	return os.Rename(source, destination)
}
