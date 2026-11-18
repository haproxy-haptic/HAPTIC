//go:build integration

package integration

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestGenerateSafeNamespaceName(t *testing.T) {
	tests := []struct {
		name     string
		testName string
	}{
		{
			name:     "short test name",
			testName: "TestSimple",
		},
		{
			name:     "medium test name with subtest",
			testName: "TestSync/add-backend",
		},
		{
			name:     "long test name that needs truncation",
			testName: "TestSync/backend-add-http-response-rule",
		},
		{
			name:     "very long test name",
			testName: "TestVeryLongTestName/with-a-very-long-subtest-name-that-definitely-exceeds-limits",
		},
		{
			name:     "test name with multiple slashes",
			testName: "TestParent/TestChild/TestGrandchild/backend-add-acl",
		},
		{
			name:     "test name with uppercase and special chars",
			testName: "TestCamelCase/Add-Backend-With-HTTP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateSafeNamespaceName(tt.testName)

			if problems := validation.IsDNS1123Label(result); len(problems) > 0 {
				t.Errorf("Generated namespace %q is invalid: %v", result, problems)
			}

			if !strings.HasPrefix(result, "test-") {
				t.Errorf("Generated namespace name '%s' doesn't start with 'test-'", result)
			}

			result2 := generateSafeNamespaceName(tt.testName)
			if result == result2 {
				t.Logf("Note: Generated names are identical (this is rare but possible): %s", result)
			}

			t.Logf("Test name: %s -> Namespace: %s (length: %d)",
				tt.testName, result, len(result))
		})
	}
}

func TestIsValidK8sName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"valid simple", "test", true},
		{"valid with hyphen", "test-name", true},
		{"valid with number", "test123", true},
		{"invalid uppercase", "TestName", false},
		{"invalid starts with hyphen", "-test", false},
		{"invalid ends with hyphen", "test-", false},
		{"invalid underscore", "test_name", false},
		{"invalid dot", "test.name", false},
		{"invalid slash", "test/name", false},
		{"invalid too long", strings.Repeat("a", 64), false},
		{"valid max length", strings.Repeat("a", 63), true},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := len(validation.IsDNS1123Label(tt.input)) == 0
			if result != tt.valid {
				t.Errorf("DNS label validity for %q = %v, want %v", tt.input, result, tt.valid)
			}
		})
	}
}
