//go:build !darwin && !linux

package trader

import (
	"context"
	"errors"
)

func lockFile(context.Context, string) (func(), error) {
	return nil, errors.New("durable process locking currently supports macOS and Linux")
}
func syncDirectory(string) error { return nil }
