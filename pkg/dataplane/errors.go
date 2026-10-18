package dataplane

import (
	"fmt"
	"strings"
)

const (
	phaseNameSyntax   = "syntax"
	phaseNameSemantic = "semantic"
)

// ValidationError represents semantic validation failure from HAProxy.
type ValidationError struct {
	// Phase indicates which validation phase failed: "syntax" or "semantic"
	Phase string

	// Message is the validation error message
	Message string

	// Cause is the underlying error
	Cause error
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if e.Phase != "" {
		return fmt.Sprintf("%s validation failed: %s: %v", e.Phase, e.Message, e.Cause)
	}
	return fmt.Sprintf("HAProxy validation failed: %s: %v", e.Message, e.Cause)
}

// Unwrap returns the underlying error for error unwrapping.
func (e *ValidationError) Unwrap() error {
	return e.Cause
}

// validationPhase pairs a phase identifier with its canonical user-facing
// message, so callers can build a ValidationError with one call instead of
// re-typing the message each time the same phase fails. The phase name is
// also part of the error string surfaced by ValidationError.Error().
type validationPhase struct {
	name    string
	message string
}

var phaseSemantic = validationPhase{name: phaseNameSemantic, message: "configuration has semantic errors"}

// wrap builds a ValidationError attributing the failure to this phase.
func (p validationPhase) wrap(cause error) *ValidationError {
	return &ValidationError{Phase: p.name, Message: p.message, Cause: cause}
}

// SimplifyValidationError parses HAProxy validation errors and extracts
// the key information for user-friendly error messages.
//
// Handles two types of validation errors:
//
//  1. Schema validation errors - OpenAPI spec violations:
//     Input: "schema validation failed: configuration violates API schema constraints: ... Error at "/field": constraint"
//     Output: "field constraint (got value)"
//
//  2. Semantic validation errors - HAProxy binary validation failures:
//     Input: "semantic validation failed: configuration has semantic errors: haproxy validation failed: <context>"
//     Output: "<context>" (preserves parseHAProxyError output with context lines)
//
// Returns original error string if parsing fails.
func SimplifyValidationError(err error) string {
	if err == nil {
		return ""
	}

	errStr := err.Error()

	// Try semantic validation error first (preserves context from parseHAProxyError)
	if strings.Contains(errStr, "semantic validation failed") {
		return simplifySemanticError(errStr)
	}

	// Try schema validation error
	if strings.Contains(errStr, "schema validation failed") {
		return simplifySchemaError(errStr)
	}

	// Unknown error type, return as-is
	return errStr
}

// simplifySemanticError extracts HAProxy semantic validation context by stripping redundant wrappers.
//
// Input format:
//
//	"semantic validation failed: configuration has semantic errors: haproxy validation failed: <context>"
//
// Output: "<context>" (the parseHAProxyError output).
func simplifySemanticError(errStr string) string {
	// Find the last "haproxy validation failed:" which precedes the actual error
	marker := "haproxy validation failed: "
	idx := strings.LastIndex(errStr, marker)
	if idx == -1 {
		// Can't find marker, return original
		return errStr
	}

	// Extract everything after the marker (the parseHAProxyError output)
	return errStr[idx+len(marker):]
}

// simplifySchemaError extracts OpenAPI schema validation constraint details by parsing error messages.
//
// Input format:
//
//	"schema validation failed: ... Error at "/field_name": constraint"
//	Value: "value"
//
// Output: "field_name constraint (got value)".
func simplifySchemaError(errStr string) string {
	// Try to extract the "Error at" line which contains the useful information
	// Format: Error at "/field_name": <constraint description>
	errorAtIndex := strings.Index(errStr, "Error at \"")
	if errorAtIndex == -1 {
		// Can't find "Error at", return original
		return errStr
	}

	// Extract from "Error at" to the end of that line
	remaining := errStr[errorAtIndex:]
	lines := strings.Split(remaining, "\n")
	if len(lines) == 0 {
		return errStr
	}

	errorLine := lines[0]

	// Parse field name: Error at "/field_name": ...
	fieldStart := strings.Index(errorLine, "\"/") + 2
	fieldEnd := strings.Index(errorLine[fieldStart:], "\"")
	if fieldEnd == -1 {
		return errStr
	}

	field := errorLine[fieldStart : fieldStart+fieldEnd]

	// Extract constraint description (after the field name)
	constraintStart := fieldStart + fieldEnd + 3 // Skip ": "
	if constraintStart >= len(errorLine) {
		return errStr
	}

	constraint := errorLine[constraintStart:]

	// Try to extract value if present
	// Format: Value:\n  "value"
	var value string
	_, after, ok := strings.Cut(remaining, "Value:\n")
	if ok {
		valueText := after // Skip "Value:\n"
		valueLines := strings.Split(valueText, "\n")
		if len(valueLines) > 0 {
			value = strings.TrimSpace(valueLines[0])
			// Remove only the outermost quotes (not escaped quotes inside)
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
		}
	}

	// Build simplified message
	var simplified string
	if value != "" {
		simplified = fmt.Sprintf("%s %s (got %s)", field, constraint, value)
	} else {
		simplified = fmt.Sprintf("%s %s", field, constraint)
	}

	return simplified
}
