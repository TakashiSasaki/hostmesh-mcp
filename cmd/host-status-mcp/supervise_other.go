//go:build !linux

package main

import (
	"context"
	"errors"
)

func runSupervise(context.Context, []string) error {
	return errors.New("supervise requires Linux")
}
