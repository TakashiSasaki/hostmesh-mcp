package mcpserver

import (
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func HTTP(server *mcp.Server) http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && !strings.EqualFold(origins[0], "http://"+r.Host) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}
