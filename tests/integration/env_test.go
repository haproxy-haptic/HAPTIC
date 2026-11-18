//go:build integration

package integration

import (
	"encoding/hex"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestGenerateSafeNamespaceName(t *testing.T) {
	// A 63-byte label leaves 49 bytes between "test-" and the nine-byte hash suffix.
	maxBase := strings.Repeat("a", 49)
	tests := []struct {
		name      string
		input     string
		wantBase  string
		wantValid bool
	}{
		{"short", "TestSimple", "testsimple", true},
		{"subtest", "TestSync/add-backend", "testsync-add-backend", true},
		{"nested rule", "TestSync/backend-add-http-response-rule", "testsync-backend-add-http-response-rule", true},
		{"long subtest", "TestVeryLongTestName/with-a-very-long-subtest-name-that-definitely-exceeds-limits", "testverylongtestname-with-a-very-long-subtest-nam", true},
		{"multiple slashes", "TestParent/TestChild/TestGrandchild/backend-add-acl", "testparent-testchild-testgrandchild-backend-add-a", true},
		{"mixed case", "TestCamelCase/Add-Backend-With-HTTP", "testcamelcase-add-backend-with-http", true},
		{"simple", "test", "test", true},
		{"hyphen", "test-name", "test-name", true},
		{"number", "test123", "test123", true},
		{"uppercase", "TestName", "testname", true},
		{"leading hyphen", "-test", "-test", true},
		{"trailing hyphen", "test-", "test-", true},
		{"underscore", "test_name", "test_name", false},
		{"dot", "test.name", "test.name", false},
		{"slash", "test/name", "test-name", true},
		{"over label limit", strings.Repeat("a", 64), maxBase, true},
		{"at label limit", strings.Repeat("a", 63), maxBase, true},
		{"empty", "", "", true},
		{"at base limit", maxBase, maxBase, true},
		{"truncated after slash", strings.Repeat("a", 48) + "/child", strings.Repeat("a", 48) + "-", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := generateSafeNamespaceName(tt.input)
			suffix, ok := strings.CutPrefix(name, "test-"+tt.wantBase+"-")
			if !ok {
				t.Fatalf("namespace %q has unexpected base, want %q", name, tt.wantBase)
			}
			if _, err := hex.DecodeString(suffix); err != nil || len(suffix) != 8 {
				t.Errorf("namespace %q has invalid hash suffix %q", name, suffix)
			}
			problems := validation.IsDNS1123Label(name)
			if (len(problems) == 0) != tt.wantValid {
				t.Errorf("namespace %q: want valid=%t, validation errors=%v", name, tt.wantValid, problems)
			}
		})
	}
}
