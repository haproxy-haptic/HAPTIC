package buildinfo

import "errors"

// Identity identifies architecture-independent inputs to a controller image.
type Identity struct {
	Version        string `json:"version"`
	SourceHash     string `json:"sourceHash"`
	HAProxyVersion string `json:"haproxyVersion"`
}

func (i Identity) Validate() error {
	for _, value := range []string{i.Version, i.SourceHash, i.HAProxyVersion} {
		if value == "" || value == "unknown" {
			return errors.New("controller build identity is unavailable; use an image built by the HAPTIC build targets")
		}
	}
	return nil
}
