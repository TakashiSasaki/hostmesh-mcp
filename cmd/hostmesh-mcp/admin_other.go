//go:build !linux

package main

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (a *adminServer) addLinux(s *mcp.Server) {}
