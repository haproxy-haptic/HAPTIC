package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
)

func TestBuildIdentityOnlyExposesImmutableMetadata(t *testing.T) {
	identity := buildinfo.Identity{Version: "0.7.0", SourceHash: "source", HAProxyVersion: "3.3"}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		request := httptest.NewRequest(method, "/build", http.NoBody)
		request.RemoteAddr = "192.0.2.10:1234"
		response := httptest.NewRecorder()
		buildIdentityHandler(identity)(response, request)
		if method != http.MethodGet {
			assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
			continue
		}
		require.Equal(t, http.StatusOK, response.Code)
		var payload map[string]string
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
		assert.Equal(t, map[string]string{"version": "0.7.0", "sourceHash": "source", "haproxyVersion": "3.3"}, payload)
	}
}
