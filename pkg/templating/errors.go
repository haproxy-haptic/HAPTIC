package templating

import (
	"errors"
	"fmt"
)

// CompilationError represents a template compilation failure.
// This error occurs during template initialization when the template
// syntax is invalid or contains unsupported constructs.
type CompilationError struct {
	// TemplateName is the name of the template that failed to compile
	TemplateName string

	// TemplateSnippet contains the first 200 characters of the template
	TemplateSnippet string

	// Cause is the underlying compilation error from the template engine
	Cause error
}

// Error implements the error interface.
func (e *CompilationError) Error() string {
	return fmt.Sprintf("compiling template '%s': %v", e.TemplateName, e.Cause)
}

// Unwrap returns the underlying cause for error unwrapping.
func (e *CompilationError) Unwrap() error {
	return e.Cause
}

// RenderError represents a template rendering failure.
// This error occurs when a valid template fails during execution,
// typically due to missing context variables or runtime evaluation errors.
type RenderError struct {
	// TemplateName is the name of the template that failed to render
	TemplateName string

	// Cause is the underlying rendering error from the template engine
	Cause error
}

// Error implements the error interface.
func (e *RenderError) Error() string {
	return fmt.Sprintf("rendering template '%s': %v", e.TemplateName, e.Cause)
}

// Unwrap returns the underlying cause for error unwrapping.
func (e *RenderError) Unwrap() error {
	return e.Cause
}

// TemplateNotFoundError represents a request for a non-existent template.
type TemplateNotFoundError struct {
	// TemplateName is the name of the requested template
	TemplateName string

	// AvailableTemplates lists all available template names
	AvailableTemplates []string
}

// Error implements the error interface.
func (e *TemplateNotFoundError) Error() string {
	return fmt.Sprintf("template '%s' not found", e.TemplateName)
}

// RenderTimeoutError represents a template rendering that exceeded the context deadline.
type RenderTimeoutError struct {
	// TemplateName is the name of the template that timed out
	TemplateName string

	// Cause is the underlying context error
	Cause error
}

// Error implements the error interface.
func (e *RenderTimeoutError) Error() string {
	return fmt.Sprintf("template '%s' render timed out: %v", e.TemplateName, e.Cause)
}

// Unwrap returns the underlying cause for error unwrapping.
func (e *RenderTimeoutError) Unwrap() error {
	return e.Cause
}

// FailError is the error fail() stops a render with. Message is the template's
// text for the operator.
type FailError struct {
	Message string
}

// Error implements the error interface.
func (e *FailError) Error() string {
	return e.Message
}

// RenderErrorMessage returns the message a template passed to fail(), or err's
// full text when the render failed for another reason.
func RenderErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if failErr, ok := errors.AsType[*FailError](err); ok {
		return failErr.Message
	}
	return err.Error()
}

// Helper functions for creating errors with actionable context

// NewCompilationError creates a CompilationError for a template compilation failure.
func NewCompilationError(templateName, templateContent string, cause error) *CompilationError {
	snippet := templateContent
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}

	return &CompilationError{
		TemplateName:    templateName,
		TemplateSnippet: snippet,
		Cause:           cause,
	}
}

// NewRenderError creates a RenderError for a template rendering failure.
func NewRenderError(templateName string, cause error) *RenderError {
	return &RenderError{
		TemplateName: templateName,
		Cause:        cause,
	}
}

// NewTemplateNotFoundError creates a TemplateNotFoundError with the list of available templates.
func NewTemplateNotFoundError(templateName string, availableTemplates []string) *TemplateNotFoundError {
	return &TemplateNotFoundError{
		TemplateName:       templateName,
		AvailableTemplates: availableTemplates,
	}
}
