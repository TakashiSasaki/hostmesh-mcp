package main

import (
	"bytes"
	"context"
	"runtime"
	"strings"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/ghrelease"
)

func githubRelease(ctx context.Context) (string, releaseArtifact, error) {
	v, a, err := ghrelease.Latest(ctx, runtime.GOARCH, func(ctx context.Context, url string, limit int64) ([]byte, error) {
		var b bytes.Buffer
		_, err := fetchUpdate(ctx, url, &b, limit)
		return b.Bytes(), err
	})
	return v, releaseArtifact{URL: a.URL, Size: a.Size, SHA256: strings.TrimPrefix(a.Digest, "sha256:")}, err
}
