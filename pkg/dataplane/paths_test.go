package dataplane

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePaths(t *testing.T) {
	basePath := PathConfig{
		MapsDir:    "/etc/haproxy/maps",
		SSLDir:     "/etc/haproxy/ssl",
		GeneralDir: "/etc/haproxy/files",
		ConfigFile: "/etc/haproxy/haproxy.cfg",
	}

	// CRT-list files are always stored under GeneralDir, because the native
	// CRT-list API triggers a reload on create without a skip_reload
	// parameter.
	resolved := ResolvePaths(&basePath)

	require.NotNil(t, resolved)
	assert.Equal(t, "/etc/haproxy/maps", resolved.MapsDir)
	assert.Equal(t, "/etc/haproxy/ssl", resolved.SSLDir)
	assert.Equal(t, "/etc/haproxy/files", resolved.GeneralDir)
	assert.Equal(t, "/etc/haproxy/haproxy.cfg", resolved.ConfigFile)
	assert.Equal(t, "/etc/haproxy/files", resolved.CRTListDir,
		"CRTListDir should always be GeneralDir")
}

func TestResolvedPaths_ToValidationPaths(t *testing.T) {
	resolved := &ResolvedPaths{
		MapsDir:    "/tmp/haproxy-validate-12345/maps",
		SSLDir:     "/tmp/haproxy-validate-12345/ssl",
		CRTListDir: "/tmp/haproxy-validate-12345/crtlist",
		GeneralDir: "/tmp/haproxy-validate-12345/general",
		ConfigFile: "/tmp/haproxy-validate-12345/haproxy.cfg",
	}

	validationPaths := resolved.ToValidationPaths()

	require.NotNil(t, validationPaths)
	// TempDir is derived from ConfigFile's parent directory via filepath.Dir,
	// which uses the host OS separator (backslashes on Windows).
	assert.Equal(t, filepath.FromSlash("/tmp/haproxy-validate-12345"), validationPaths.TempDir)
	assert.Equal(t, "/tmp/haproxy-validate-12345/maps", validationPaths.MapsDir)
	assert.Equal(t, "/tmp/haproxy-validate-12345/ssl", validationPaths.SSLCertsDir)
	assert.Equal(t, "/tmp/haproxy-validate-12345/crtlist", validationPaths.CRTListDir)
	assert.Equal(t, "/tmp/haproxy-validate-12345/general", validationPaths.GeneralStorageDir)
	assert.Equal(t, "/tmp/haproxy-validate-12345/haproxy.cfg", validationPaths.ConfigFile)
}
