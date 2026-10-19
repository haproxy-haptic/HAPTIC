package controller

import (
	"net/http"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
)

var buildSourceHash = "unknown"

// SetBuildSourceHash sets the immutable source identity before Run starts.
func SetBuildSourceHash(value string) { buildSourceHash = value }

func registerBuildIdentity(server *introspection.Server) {
	identity := buildinfo.Identity{
		Version: buildVersionInfo.version, SourceHash: buildSourceHash,
		HAProxyVersion: buildVersionInfo.haproxyVersion,
	}
	server.RegisterHandler("/build", buildIdentityHandler(identity))
}

func buildIdentityHandler(identity buildinfo.Identity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			introspection.WriteError(w, http.StatusMethodNotAllowed, "only GET is allowed")
			return
		}
		introspection.WriteJSON(w, identity)
	}
}
