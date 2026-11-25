//go:build !linux

package main

import (
	"context"
	"errors"
)

func githubRelease(context.Context) (string, releaseArtifact, error) {
	return "", releaseArtifact{}, errors.New("binary updates require Linux")
}
