//go:build !darwin && !linux

package store

import (
	"context"
	"errors"
)

func LockFile(context.Context, string) (func(), error) {
	return nil, errors.New("durable process locking currently supports macOS and Linux")
}
func syncDirectory(string) error { return nil }
