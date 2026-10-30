package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestDashboardMetricsAreRegistered(t *testing.T) {
	registry := prometheus.NewRegistry()
	NewMetrics(registry)
	descriptions := make(chan *prometheus.Desc)
	go func() {
		registry.Describe(descriptions)
		close(descriptions)
	}()
	registered := make(map[string]bool)
	descriptorName := regexp.MustCompile(`fqName: "([^"]+)"`)
	var descriptorStrings []string
	for description := range descriptions {
		descriptorStrings = append(descriptorStrings, description.String())
	}
	for _, description := range descriptorStrings {
		match := descriptorName.FindStringSubmatch(description)
		require.Len(t, match, 2)
		registered[match[1]] = true
	}
	dashboard, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "haptic", "dashboards", "haptic-controller.json"))
	require.NoError(t, err)
	references := regexp.MustCompile(`\bhaptic_[a-z0-9_]+\b`).FindAllString(string(dashboard), -1)
	require.NotEmpty(t, references)
	for _, name := range references {
		base := name
		if !registered[base] {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if strings.HasSuffix(base, suffix) {
					base = strings.TrimSuffix(base, suffix)
					break
				}
			}
		}
		require.True(t, registered[base], "dashboard references unregistered controller metric %s", name)
	}
}
