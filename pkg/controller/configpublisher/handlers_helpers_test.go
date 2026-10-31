// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

// convertAuxiliaryFiles is the type-bridge that pivots from the
// dataplane.AuxiliaryFiles aggregate the renderer publishes to the
// k8s/configpublisher.AuxiliaryFiles aggregate the publisher needs.
// Both shapes are nearly identical, but the bridge has TWO
// non-obvious contracts and NO direct test coverage:
//
//  1. nil input MUST return nil (callers branch on nil for the
//     "no aux files" case). A regression that returned a non-nil
//     empty struct would silently flip those branches and try to
//     publish empty resource lists.
//
//  2. ALL FIVE aux-file slice fields must round-trip: MapFiles,
//     SSLCertificates, SSLCaFiles, GeneralFiles, CRTListFiles.
//     Adding a new field to dataplane.AuxiliaryFiles without also
//     wiring it through this converter would silently drop those
//     files from the published K8s resources — the kind of bug
//     that ships fine in unit tests against a single-aux-file
//     fixture and surfaces only in integration.
//
// Pin both contracts.
func TestComponent_ConvertAuxiliaryFiles(t *testing.T) {
	c := &Component{}

	t.Run("nil input returns nil (callers branch on this)", func(t *testing.T) {
		got := c.convertAuxiliaryFiles(nil)
		assert.Nil(t, got,
			"nil input must return nil; a non-nil empty struct would silently flip "+
				"caller branches that test for the 'no aux files' case")
	})

	t.Run("empty input returns non-nil with empty slices", func(t *testing.T) {
		// Distinct from nil: an empty AuxiliaryFiles is a valid
		// "I have no files but I checked" signal. Must NOT be
		// coerced back to nil.
		got := c.convertAuxiliaryFiles(&dataplane.AuxiliaryFiles{})
		require.NotNil(t, got,
			"empty (but non-nil) AuxiliaryFiles must round-trip to a non-nil empty struct")
		assert.Empty(t, got.MapFiles)
		assert.Empty(t, got.SSLCertificates)
		assert.Empty(t, got.SSLCaFiles)
		assert.Empty(t, got.GeneralFiles)
		assert.Empty(t, got.CRTListFiles)
	})

	t.Run("all five fields round-trip", func(t *testing.T) {
		// Plant a single distinguishable item in each of the five
		// fields. If a future refactor adds a sixth field to
		// dataplane.AuxiliaryFiles without also wiring it through
		// the converter, this assertion at least guards the
		// existing five from regression.
		input := &dataplane.AuxiliaryFiles{
			MapFiles:        []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "m"}},
			SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/ssl/cert.pem", Content: "c"}},
			SSLCaFiles:      []auxiliaryfiles.SSLCaFile{{Path: "/ssl/ca.pem", Content: "ca"}},
			GeneralFiles:    []auxiliaryfiles.GeneralFile{{Filename: "400.http", Content: "g"}},
			CRTListFiles:    []auxiliaryfiles.CRTListFile{{Path: "/crts/list.txt", Content: "l"}},
		}

		got := c.convertAuxiliaryFiles(input)
		require.NotNil(t, got)

		// Use Equal on each field — a refactor that swapped two
		// fields (e.g. assigned MapFiles to GeneralFiles) would
		// pass the Len check but fail Equal.
		assert.Equal(t, input.MapFiles, got.MapFiles, "MapFiles must round-trip")
		assert.Equal(t, input.SSLCertificates, got.SSLCertificates, "SSLCertificates must round-trip")
		assert.Equal(t, input.SSLCaFiles, got.SSLCaFiles, "SSLCaFiles must round-trip")
		assert.Equal(t, input.GeneralFiles, got.GeneralFiles, "GeneralFiles must round-trip")
		assert.Equal(t, input.CRTListFiles, got.CRTListFiles, "CRTListFiles must round-trip")
	})
}

// getCompressionThreshold has a load-bearing zero-default:
// CompressionThreshold == 0 from the CRD MUST be replaced with the
// package-level DefaultCompressionThreshold (1 MiB), NOT passed
// through as zero. A regression that dropped the guard would:
//
//   - Produce a 0-byte threshold when the CRD doesn't set it (the
//     common case), causing every config larger than 0 bytes to be
//     compressed — that's every config — when the design intent is
//     to compress only large configs (>1 MiB by default).
//   - Or, if downstream code interprets 0 as "compression disabled",
//     produce the OPPOSITE bug: never compressing anything even
//     for huge configs.
//
// Either way, silent. Pin both branches: explicit non-zero CRD
// value passes through verbatim; zero (i.e. unset / default) is
// replaced with DefaultCompressionThreshold.
func TestComponent_GetCompressionThreshold(t *testing.T) {
	c := &Component{}

	tests := []struct {
		name     string
		crdValue int64
		want     int64
	}{
		{
			name:     "CRD value 0 (unset) is replaced with the package default",
			crdValue: 0,
			want:     config.DefaultCompressionThreshold,
		},
		{
			name:     "non-zero CRD value passes through verbatim (not coerced)",
			crdValue: 524288, // 512 KiB
			want:     524288,
		},
		{
			name:     "very small non-zero CRD value (1 byte) is honoured exactly",
			crdValue: 1,
			want:     1,
		},
		{
			name:     "very large non-zero CRD value passes through (no clamping)",
			crdValue: 1024 * 1024 * 1024, // 1 GiB
			want:     1024 * 1024 * 1024,
		},
		// Note: we don't test negative values because CompressionThreshold
		// is documented as "set to 0 or negative to disable compression",
		// suggesting negative is a legitimate caller-controlled value.
		// Pinning behaviour for negatives would over-specify the API.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &v1alpha1.HAProxyTemplateConfig{
				Spec: v1alpha1.HAProxyTemplateConfigSpec{
					Controller: v1alpha1.ControllerConfig{
						ConfigPublishing: v1alpha1.ConfigPublishingConfig{
							CompressionThreshold: tt.crdValue,
						},
					},
				},
			}
			got := c.getCompressionThreshold(tc)
			assert.Equal(t, tt.want, got,
				"CRD value %d must yield %d (note: zero means 'use default', not 'disable')",
				tt.crdValue, tt.want)
		})
	}
}

// TestComponent_ConvertAuxiliaryFiles_StructShape asserts via reflect
// that configpublisher.AuxiliaryFiles has exactly the six fields the
// converter knows about. The original Gitar review pointed out that a
// keyed struct literal only catches field removals/renames, not
// additions — Go happily compiles a keyed literal even when the
// struct grows a new field. Unkeyed positional literals would enforce
// arity in both directions but trip govet's "composites" check.
//
// Reflect-based runtime check sidesteps both problems: it counts the
// fields directly, so adding OR removing any field forces the test
// to be updated alongside convertAuxiliaryFiles.
func TestComponent_ConvertAuxiliaryFiles_StructShape(t *testing.T) {
	typ := reflect.TypeOf(configpublisher.AuxiliaryFiles{})
	require.Equal(t, 6, typ.NumField(),
		"configpublisher.AuxiliaryFiles must have exactly 6 fields "+
			"(MapFiles, SSLCertificates, SSLCaFiles, GeneralFiles, CRTListFiles, SecretFiles); "+
			"if you added or removed a field, also update convertAuxiliaryFiles in handlers.go "+
			"to wire the new field through (or drop it from) the type-bridge — otherwise "+
			"newly-added aux files would be silently dropped from published K8s resources")

	// Pin the exact field names too so a rename also forces a
	// review of the converter (rename without converter update
	// would silently route data to the wrong destination field).
	wantFields := map[string]bool{
		"MapFiles": true, "SSLCertificates": true, "SSLCaFiles": true,
		"GeneralFiles": true, "CRTListFiles": true, "SecretFiles": true,
	}
	gotFields := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		gotFields[typ.Field(i).Name] = true
	}
	assert.Equal(t, wantFields, gotFields,
		"a field rename must update convertAuxiliaryFiles to route through the new name")
}
