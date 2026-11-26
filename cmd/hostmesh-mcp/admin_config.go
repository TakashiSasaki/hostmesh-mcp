package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/localfile"
)

type configEdit struct {
	Kind     string         `json:"kind" jsonschema:"peer or ca; updater on Linux"`
	Config   map[string]any `json:"config" jsonschema:"Complete configuration JSON object. Use an example or the object returned by config_read."`
	Expected string         `json:"expected_sha256,omitempty" jsonschema:"Current file revision from config_read when replacing an existing configuration"`
}

func (a *adminServer) readConfiguration(_ context.Context, in configKind) (adminResult, error) {
	path, err := a.configPath(in.Kind)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, errors.New("expected a configuration JSON object")
	}
	return adminResult{"path": path, "config": doc, "sha256": fmt.Sprintf("%x", sha256.Sum256(data))}, nil
}

func (a *adminServer) saveConfiguration(_ context.Context, in configEdit) (adminResult, error) {
	path, err := a.configPath(in.Kind)
	if err != nil {
		return nil, err
	}
	if in.Config == nil {
		return nil, errors.New("expected a configuration object")
	}
	data, err := json.MarshalIndent(in.Config, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	a.configMu.Lock()
	defer a.configMu.Unlock()
	// The stable sibling lock also serializes independent administration processes.
	lock, err := os.OpenFile(path+".admin.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = localfile.Lock(lock, true, true); err != nil {
		return nil, err
	}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if in.Expected != "" {
			return nil, errors.New("configuration no longer exists")
		}
	case err != nil:
		return nil, err
	case fmt.Sprintf("%x", sha256.Sum256(old)) != in.Expected:
		return nil, errors.New("configuration revision mismatch; read the file before replacing it")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hostmesh-config-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Sync(), tmp.Close())
	if err != nil {
		return nil, err
	}
	if err = checkAdminConfig(in.Kind, tmp.Name()); err != nil {
		return nil, err
	}
	if in.Expected == "" {
		err = os.Link(tmp.Name(), path)
	} else {
		err = localfile.Replace(tmp.Name(), path)
	}
	if err != nil {
		return nil, err
	}
	fd, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	if err = localfile.SyncDir(fd); err != nil {
		return nil, err
	}
	return adminResult{"path": path, "sha256": fmt.Sprintf("%x", sha256.Sum256(data))}, nil
}
