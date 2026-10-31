package dataplane

import (
	"cmp"
	"path"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// Endpoint is one HAProxy pod's agent: where to reach it and which pod it is.
type Endpoint struct {
	// URL is the agent endpoint (e.g., "http://haproxy:5555").
	URL string

	// Username for basic authentication
	Username string

	// Password for basic authentication
	Password string

	// PodName is the Kubernetes pod name (for observability)
	PodName string

	// PodNamespace is the Kubernetes pod namespace (for observability)
	PodNamespace string

	// PodUID distinguishes replacements that reuse the same namespace and name.
	PodUID string

	// PodRuntimeID distinguishes container restarts and image changes within one pod UID.
	PodRuntimeID string

	// HAProxy version the pod's agent reported, cached after discovery
	// admission. It decides this pod's runtime capabilities and, at its fleet
	// minimum, the template ones. Zero values mean not yet detected.
	DetectedMajorVersion int    // Major version (e.g., 3)
	DetectedMinorVersion int    // Minor version (e.g., 4)
	DetectedFullVersion  string // Full version string (e.g., "3.4.3")
}

// HasCachedVersion returns true if version info has been cached on this endpoint.
func (e *Endpoint) HasCachedVersion() bool {
	return e.DetectedMajorVersion > 0
}

// AuxiliaryFiles are the files a render produces next to haproxy.cfg. They
// travel to the pod in the same apply as the configuration that names them.
type AuxiliaryFiles struct {
	// GeneralFiles contains general-purpose files (error pages, custom response files, etc.)
	GeneralFiles []auxiliaryfiles.GeneralFile

	// SSLCertificates contains SSL certificates to sync to HAProxy SSL storage
	SSLCertificates []auxiliaryfiles.SSLCertificate

	// SSLCaFiles contains SSL CA certificate files for client/backend certificate verification
	SSLCaFiles []auxiliaryfiles.SSLCaFile

	// MapFiles contains map files for backend routing and other map-based features
	MapFiles []auxiliaryfiles.MapFile

	// CRTListFiles contains crt-list files for SSL certificate lists with per-certificate options
	CRTListFiles []auxiliaryfiles.CRTListFile

	// SecretFiles contains sidecar files that are published as Kubernetes Secrets
	SecretFiles []auxiliaryfiles.SecretFile
}

// CurrentFiles projects CRD-backed auxiliary output into the currentFiles map.
// Secret-backed certificate, CA and secret-file content is deliberately excluded.
func (af *AuxiliaryFiles) CurrentFiles() map[string]string {
	if af == nil {
		return nil
	}
	m := make(map[string]string, len(af.MapFiles)+len(af.GeneralFiles)+len(af.CRTListFiles))
	for _, f := range af.MapFiles {
		m[path.Base(f.Path)] = f.Content
	}
	for _, f := range af.GeneralFiles {
		if f.IsCaFile {
			continue
		}
		m[path.Base(f.Path)] = f.Content
	}
	for _, f := range af.CRTListFiles {
		m[path.Base(f.Path)] = f.Content
	}
	return m
}

// Sort sorts all auxiliary file slices in-place by their path/filename.
// This establishes a deterministic order so that downstream consumers
// (checksum, diff, etc.) can iterate directly without cloning or sorting.
func (a *AuxiliaryFiles) Sort() {
	slices.SortFunc(a.GeneralFiles, func(x, y auxiliaryfiles.GeneralFile) int {
		return cmp.Compare(x.Filename, y.Filename)
	})
	slices.SortFunc(a.SSLCertificates, func(x, y auxiliaryfiles.SSLCertificate) int {
		return cmp.Compare(x.Path, y.Path)
	})
	slices.SortFunc(a.SSLCaFiles, func(x, y auxiliaryfiles.SSLCaFile) int {
		return cmp.Compare(x.Path, y.Path)
	})
	slices.SortFunc(a.MapFiles, func(x, y auxiliaryfiles.MapFile) int {
		return cmp.Compare(x.Path, y.Path)
	})
	slices.SortFunc(a.CRTListFiles, func(x, y auxiliaryfiles.CRTListFile) int {
		return cmp.Compare(x.Path, y.Path)
	})
	slices.SortFunc(a.SecretFiles, func(x, y auxiliaryfiles.SecretFile) int {
		return cmp.Compare(x.Path, y.Path)
	})
}

// DefaultAuxiliaryFiles returns an empty auxiliary files struct.
func DefaultAuxiliaryFiles() *AuxiliaryFiles {
	return &AuxiliaryFiles{}
}
