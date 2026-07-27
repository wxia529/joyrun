package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

func New(prefix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%x_%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(random[:])), nil
}
