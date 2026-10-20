package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckRenderedSidecarConfigsRestrictiveUmask(t *testing.T) {
	for _, tc := range []struct {
		name string
		mask int
	}{
		{name: "027", mask: 0o027},
		{name: "077", mask: 0o077},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Umask is process-wide, so these cases must remain sequential.
			previous := unix.Umask(tc.mask)
			t.Cleanup(func() { unix.Umask(previous) })
			checkSidecarCompilers(t)
		})
	}
}
