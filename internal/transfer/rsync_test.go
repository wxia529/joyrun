package transfer

import (
	"strings"
	"testing"
)

func TestRsyncArgsBoundIdleTransfersAndUseHardenedSSH(t *testing.T) {
	args := baseArgs()
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"--timeout=90",
		"ConnectTimeout=15",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing rsync safeguard %q in %q", expected, joined)
		}
	}
}
