// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dataplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// defaultCheckGate serializes the checks of every caller that does not bring
// its own gate — the admission webhook, the startup load gate, the test runner.
var defaultCheckGate = NewCheckGate(0)

// ErrHAProxyRefused reports that the haproxy binary judged a configuration and
// refused it. A check that could not run at all (no binary, unwritable temp
// tree, cancelled) does not wrap it, so callers never read an infrastructure
// failure as a verdict on the config.
var ErrHAProxyRefused = errors.New("haproxy validation failed")

// RefusalError is a configuration refusal by the haproxy binary. It wraps ErrHAProxyRefused.
type RefusalError struct {
	// Causes are HAProxy's failure messages without process IDs, line numbers, or the
	// check's working directory, so refusals of different renders compare by cause.
	Causes []string

	message string
}

func (e *RefusalError) Error() string { return ErrHAProxyRefused.Error() + ": " + e.message }

func (e *RefusalError) Unwrap() error { return ErrHAProxyRefused }

var (
	failureLinePrefix  = regexp.MustCompile(`^\[(?:EMERG|ALERT|CRIT|ERR)\][^:]*:\s*`)
	configLinePosition = regexp.MustCompile(`(\[[^\]\s:]+):\d+\]`)
)

func refusalCauses(output, workDir string) []string {
	var causes []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !failureLinePrefix.MatchString(trimmed) || shouldSkipLine(trimmed) {
			continue
		}
		cause := failureLinePrefix.ReplaceAllString(trimmed, "")
		if workDir != "" {
			cause = strings.ReplaceAll(cause, workDir+string(filepath.Separator), "")
		}
		causes = append(causes, configLinePosition.ReplaceAllString(cause, "$1]"))
	}
	return causes
}

// validateSemantics performs semantic validation using haproxy binary.
// This writes files to actual /etc/haproxy/ directories and runs haproxy -c.
// If skipDNSValidation is true, the -dr flag is passed to HAProxy to skip DNS resolution
// failures (servers with unresolvable hostnames start in DOWN state instead of failing).
// gate serializes the check; nil uses defaultCheckGate. verdicts, when non-nil,
// answers an input set it has already judged; reused reports that it did.
func validateSemantics(ctx context.Context, mainConfig string, auxFiles *AuxiliaryFiles, paths *ValidationPaths, skipDNSValidation bool, gate *CheckGate, verdicts *CheckVerdicts) (reused bool, err error) {
	if cause := context.Cause(ctx); cause != nil {
		return false, cause
	}
	key, reusable := verdicts.key(mainConfig, auxFiles, paths, skipDNSValidation)
	if reusable {
		if verdict, found := verdicts.lookup(key); found {
			return true, verdict.err
		}
	}
	err = runSemanticCheck(ctx, mainConfig, auxFiles, paths, skipDNSValidation, gate)
	if reusable && context.Cause(ctx) == nil && (err == nil || errors.Is(err, ErrHAProxyRefused)) {
		verdicts.remember(key, checkVerdict{err: err})
	}
	return false, err
}

func runSemanticCheck(ctx context.Context, mainConfig string, auxFiles *AuxiliaryFiles, paths *ValidationPaths, skipDNSValidation bool, gate *CheckGate) error {
	// Timing for file I/O setup vs haproxy check
	var clearMs, writeAuxMs, writeConfigMs, haproxyCheckMs int64

	// Clear validation directories to remove any pre-existing files
	clearStart := time.Now()
	if err := clearValidationDirectories(paths); err != nil {
		return fmt.Errorf("clearing validation directories: %w", err)
	}
	clearMs = time.Since(clearStart).Milliseconds()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	// Write auxiliary files to their respective directories
	writeAuxStart := time.Now()
	if err := writeAuxiliaryFiles(auxFiles, paths); err != nil {
		return fmt.Errorf("writing auxiliary files: %w", err)
	}
	writeAuxMs = time.Since(writeAuxStart).Milliseconds()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	// Write main configuration to ConfigFile path
	writeConfigStart := time.Now()
	if err := os.WriteFile(paths.ConfigFile, []byte(mainConfig), 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	writeConfigMs = time.Since(writeConfigStart).Milliseconds()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	// Run haproxy -c -f <ConfigFile>
	haproxyCheckStart := time.Now()
	if err := runHAProxyCheck(ctx, paths.ConfigFile, mainConfig, skipDNSValidation, gate); err != nil {
		return err
	}
	haproxyCheckMs = time.Since(haproxyCheckStart).Milliseconds()

	// Log semantic validation timing breakdown
	slog.Debug("Semantic validation timing breakdown",
		"clear_dirs_ms", clearMs,
		"write_aux_ms", writeAuxMs,
		"write_config_ms", writeConfigMs,
		"haproxy_check_ms", haproxyCheckMs,
	)

	return nil
}

// clearValidationDirectories removes all files from validation directories.
// This ensures no pre-existing files interfere with validation.
// It clears both the traditional validation directories (for absolute/simple paths)
// and subdirectories in the config directory (for relative paths with subdirectories).
func clearValidationDirectories(paths *ValidationPaths) error {
	configDir := filepath.Dir(paths.ConfigFile)

	// Clear traditional validation directories (for absolute paths and simple filenames)
	dirs := []string{
		paths.MapsDir,
		paths.SSLCertsDir,
		paths.GeneralStorageDir,
	}

	for _, dir := range dirs {
		if err := clearDirectory(dir); err != nil {
			return err
		}
	}

	// Create config directory if it doesn't exist
	// (No need to clear it - we already cleared the specific validation directories above)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		return fmt.Errorf("creating config directory %s: %w", configDir, err)
	}

	if err := os.Remove(paths.ConfigFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing old config file: %w", err)
	}

	return nil
}

// clearDirectory creates a directory and removes all its contents.
// Uses retry logic to handle race conditions where the directory is deleted
// between MkdirAll and ReadDir (e.g., by concurrent cleanup).
func clearDirectory(dir string) error {
	var entries []os.DirEntry
	for attempt := range 2 {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}

		var err error
		entries, err = os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) && attempt == 0 {
				// Directory was deleted between MkdirAll and ReadDir
				// (race with concurrent cleanup), retry once
				continue
			}
			return fmt.Errorf("reading directory %s: %w", dir, err)
		}
		break // Success
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}

	return nil
}

// resolveAuxiliaryFilePath determines the full path for an auxiliary file and
// rejects any path that escapes the base directory it resolves under.
// It handles three cases:
// - Absolute paths: Extract filename and use fallback directory (for validation with temp directories).
// - Relative paths with subdirectories (e.g., "maps/hosts.map"): resolved relative to config directory.
// - Simple filenames: written to the specified fallback directory.
func resolveAuxiliaryFilePath(filePath, configDir, fallbackDir string) (string, error) {
	var base, resolved string
	switch {
	case filepath.IsAbs(filePath):
		// Absolute path - extract filename and use fallback directory
		// This allows validation to work with temp directories instead of production paths
		// Example: /etc/haproxy/ssl/cert.pem → <tmpdir>/ssl/cert.pem
		base, resolved = fallbackDir, filepath.Join(fallbackDir, filepath.Base(filePath))
	case strings.Contains(filePath, string(filepath.Separator)):
		// Relative path with subdirectory - resolve relative to config directory
		base, resolved = configDir, filepath.Join(configDir, filePath)
	default:
		// Just a filename - write to fallback directory
		base, resolved = fallbackDir, filepath.Join(fallbackDir, filePath)
	}

	if err := ensureContainedPath(base, resolved); err != nil {
		return "", fmt.Errorf("auxiliary file %q: %w", filePath, err)
	}
	return resolved, nil
}

// ensureContainedPath returns an error when target resolves outside base.
// Both are compared after cleaning, so a "../" climb is caught even when it is
// buried mid-path or expressed as a lone "..".
func ensureContainedPath(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return fmt.Errorf("resolving path under %q: %w", base, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes %q", target, base)
	}
	return nil
}

// writeFileWithDir writes a file to disk, creating parent directories if needed.
func writeFileWithDir(path, content, fileType string) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", fileType, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", fileType, err)
	}

	return nil
}

// writeAuxiliaryFiles writes all auxiliary files to their respective directories.
func writeAuxiliaryFiles(auxFiles *AuxiliaryFiles, paths *ValidationPaths) error {
	return eachAuxiliaryFile(auxFiles, paths, writeFileWithDir)
}

// eachAuxiliaryFile resolves every auxiliary file to the path it is written
// to, in write order: a later file at the same path replaces an earlier one.
func eachAuxiliaryFile(auxFiles *AuxiliaryFiles, paths *ValidationPaths, visit func(resolved, content, label string) error) error {
	if auxFiles == nil {
		return nil
	}

	configDir := filepath.Dir(paths.ConfigFile)
	each := func(filePath, content, fallbackDir, label string) error {
		resolved, err := resolveAuxiliaryFilePath(filePath, configDir, fallbackDir)
		if err != nil {
			return err
		}
		return visit(resolved, content, label)
	}

	for _, mapFile := range mapFileItems(auxFiles) {
		mapPath := mapFile.GetIdentifier()
		if err := each(mapPath, mapFile.GetContent(), paths.MapsDir, "map file "+mapPath); err != nil {
			return err
		}
	}

	// file.Path carries the full relative path ("ssl/ca-bundle.pem" for CA
	// files); Filename is the fallback for callers that only set it.
	for _, file := range auxFiles.GeneralFiles {
		pathToUse := file.Path
		if pathToUse == "" {
			pathToUse = file.Filename
		}
		if err := each(pathToUse, file.Content, paths.GeneralStorageDir, "general file "+pathToUse); err != nil {
			return err
		}
	}

	for _, cert := range auxFiles.SSLCertificates {
		if err := each(cert.Path, cert.Content, paths.SSLCertsDir, "SSL certificate "+cert.Path); err != nil {
			return err
		}
	}

	for _, caFile := range auxFiles.SSLCaFiles {
		if err := each(caFile.Path, caFile.Content, paths.SSLCertsDir, "SSL CA file "+caFile.Path); err != nil {
			return err
		}
	}

	// CRTListDir may differ from SSLCertsDir on HAProxy < 3.2.
	for _, crtList := range auxFiles.CRTListFiles {
		if err := each(crtList.Path, crtList.Content, paths.CRTListDir, "CRT-list file "+crtList.Path); err != nil {
			return err
		}
	}

	return eachSensitiveFile(auxFiles, paths, each)
}

// eachSensitiveFile visits the secret files HAProxy reads; a sidecar's never
// reach the check.
func eachSensitiveFile(auxFiles *AuxiliaryFiles, paths *ValidationPaths, each func(filePath, content, fallbackDir, label string) error) error {
	for _, secret := range auxFiles.SecretFiles {
		if !secret.HAProxyReads {
			continue
		}
		if err := each(secret.Path, secret.Content, paths.SSLCertsDir, "sensitive file "+secret.Path); err != nil {
			return err
		}
	}
	return nil
}

// runHAProxyCheck runs haproxy binary with -c flag to validate configuration.
// The configuration can reference auxiliary files using relative paths
// (e.g., maps/host.map) which will be resolved relative to the config file directory.
//
// If skipDNSValidation is true, the -dr flag is passed to HAProxy. This causes HAProxy
// to append "none" to all server resolution methods, allowing startup/validation to
// proceed even when DNS resolution fails. Servers with unresolvable hostnames will
// start in RMAINT (DOWN) state instead of causing validation failure.
//
// Execution goes through the installed HAProxyExecutor (see haproxy_exec.go)
// so unit tests can substitute a fake instead of shelling out.
func runHAProxyCheck(ctx context.Context, configPath, configContent string, skipDNSValidation bool, gate *CheckGate) error {
	if gate == nil {
		gate = defaultCheckGate
	}
	if err := gate.enter(ctx); err != nil {
		return err
	}
	defer gate.leave()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	// Get absolute path for config file
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("getting absolute config path: %w", err)
	}

	// Run haproxy with the working directory set to the config file
	// directory so relative paths inside the config resolve.
	args := checkArgs(skipDNSValidation, filepath.Base(absConfigPath))
	output, err := getHAProxyExecutor().Check(ctx, filepath.Dir(absConfigPath), args...)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		// Lookup failures (no binary on PATH) carry no haproxy output to
		// interpret — surface them directly, matching the pre-seam behavior.
		if len(output) == 0 && errors.Is(err, exec.ErrNotFound) {
			return err
		}
		return interpretHAProxyExitError(output, err, configContent, filepath.Dir(absConfigPath))
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	return nil
}

// checkArgs is the `haproxy` command line for checking configName. -dr skips
// DNS resolution failures: servers start DOWN instead of failing the check.
func checkArgs(skipDNSValidation bool, configName string) []string {
	if skipDNSValidation {
		return []string{"-dr", "-c", "-f", configName}
	}
	return []string{"-c", "-f", configName}
}

// interpretHAProxyExitError classifies a non-zero `haproxy -c` exit. Three
// outcomes:
//
//  1. Empty output (segfault, OOM-kill, signal, binary-not-found) → return a
//     wrapped error. We must NOT treat this as success: there's no advisory
//     information to evaluate, just an unexplained crash.
//
//  2. Output contains only advisory lines ([WARNING]/[NOTICE]/[INFO]/[DEBUG])
//     → return nil. Some HAProxy builds (notably AWS-LC variants) exit
//     non-zero when an ignored-keyword warning triggers, even though the
//     config loads fine; failing webhook validation on those would block
//     resource admission for an advisory message.
//
//  3. Output contains real failure lines ([ALERT]/[EMERG]/[CRIT]/[ERR]) →
//     return a validation error parsed via parseHAProxyError so the caller
//     gets a user-facing message with config-file context.
//
// Extracted so the decision logic is unit-testable without invoking the
// haproxy binary.
func interpretHAProxyExitError(output []byte, exitErr error, configContent, workDir string) error {
	trimmedOutput := strings.TrimSpace(string(output))
	if trimmedOutput == "" {
		return fmt.Errorf("haproxy exited with error but produced no output: %w", exitErr)
	}
	if !hasFailureLines(string(output)) {
		slog.Warn("HAProxy emitted advisory output (no [ALERT]) — treating validation as success",
			"output", trimmedOutput)
		return nil
	}
	message := parseHAProxyError(string(output), configContent)
	if workDir != "" {
		// The check's working directory is gone once it returns, and a reused verdict outlives it.
		message = strings.ReplaceAll(message, workDir+string(filepath.Separator), "")
	}
	return &RefusalError{Causes: refusalCauses(string(output), workDir), message: message}
}

// hasFailureLines reports whether output contains any HAProxy log line at or
// above the [ALERT] severity. HAProxy uses standard syslog severity levels in
// its `-c` output; lines like [WARNING] or [NOTICE] are advisory and don't
// indicate the config is unusable. The check is line-prefix based after
// trimming leading whitespace.
func hasFailureLines(output string) bool {
	failurePrefixes := []string{"[EMERG]", "[ALERT]", "[CRIT]", "[ERR]"}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range failurePrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				return true
			}
		}
	}
	return false
}

// parseHAProxyError parses HAProxy's error output to extract meaningful error messages with context.
// HAProxy outputs errors with [ALERT] prefix and line numbers. This function:
// 1. Captures 3 lines before/after each [ALERT] from HAProxy's output
// 2. Parses line numbers from [ALERT] messages (e.g., [haproxy.cfg:90])
// 3. Extracts and shows the corresponding lines from the config file.
func parseHAProxyError(output, configContent string) string {
	lines := strings.Split(output, "\n")

	// Find all meaningful [ALERT] line indices (skip summary alerts)
	alertIndices := findAlertIndices(lines)
	if len(alertIndices) == 0 {
		return strings.TrimSpace(output)
	}

	// Split config content into lines for context extraction
	configLines := strings.Split(configContent, "\n")

	// Extract context for each alert
	errorBlocks := extractErrorBlocks(lines, alertIndices, configLines, configContent)
	if len(errorBlocks) == 0 {
		return strings.TrimSpace(output)
	}

	// Join multiple error blocks with blank line separator
	return strings.Join(errorBlocks, "\n\n")
}

// findAlertIndices finds all meaningful [ALERT] line indices, skipping summary alerts.
func findAlertIndices(lines []string) []int {
	alertIndices := make([]int, 0, 5) // Pre-allocate for typical case of few alerts
	for i, line := range lines {
		if isRelevantAlert(line) {
			alertIndices = append(alertIndices, i)
		}
	}
	return alertIndices
}

// isRelevantAlert checks if a line contains a relevant alert (not a summary).
func isRelevantAlert(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[ALERT]") {
		return false
	}

	// Skip summary [ALERT] lines
	lineLower := strings.ToLower(trimmed)
	return !strings.Contains(lineLower, "fatal errors found in configuration") &&
		!strings.Contains(lineLower, "error(s) found in configuration file")
}

// extractErrorBlocks extracts error context blocks for each alert.
func extractErrorBlocks(lines []string, alertIndices []int, configLines []string, configContent string) []string {
	var errorBlocks []string
	for _, alertIdx := range alertIndices {
		block := buildErrorBlock(lines, alertIdx, configLines, configContent)
		if len(block) > 0 {
			errorBlocks = append(errorBlocks, strings.Join(block, "\n"))
		}
	}
	return errorBlocks
}

// buildErrorBlock builds a single error context block for an alert.
func buildErrorBlock(lines []string, alertIdx int, configLines []string, configContent string) []string {
	startIdx, endIdx := calculateContextRange(alertIdx, len(lines))

	var block []string
	var alertLine string

	// Build HAProxy output context
	for i := startIdx; i < endIdx; i++ {
		line := strings.TrimRight(lines[i], " \t\r\n")
		if shouldSkipLine(line) {
			continue
		}

		// Add arrow marker for the alert line
		if i == alertIdx {
			block = append(block, "→ "+line)
			alertLine = line
		} else {
			block = append(block, "  "+line)
		}
	}

	// Add config context if available
	if alertLine != "" && configContent != "" {
		if configContext := extractConfigContext(alertLine, configLines); configContext != "" {
			block = append(block, "", "  Config context:", configContext)
		}
	}

	return block
}

// calculateContextRange calculates the start and end indices for context lines (3 before/after).
func calculateContextRange(alertIdx, totalLines int) (start, end int) {
	start = max(alertIdx-3, 0)

	end = min(
		// +4 because we want 3 lines after (inclusive range)
		alertIdx+4, totalLines)

	return start, end
}

// shouldSkipLine checks if a line should be skipped (empty or summary line).
func shouldSkipLine(line string) bool {
	if line == "" {
		return true
	}

	lineLower := strings.ToLower(line)
	return strings.Contains(lineLower, "fatal errors found in configuration") ||
		strings.Contains(lineLower, "error(s) found in configuration file")
}

// extractConfigContext extracts configuration file context around an error line.
// It parses the line number from an [ALERT] message like "[haproxy.cfg:90]"
// and returns 3 lines before/after that line with line numbers and an arrow marker.
func extractConfigContext(alertLine string, configLines []string) string {
	// Parse line number from [ALERT] message
	// Format: [ALERT] ... : config : [haproxy.cfg:90] : ...
	// or: [ALERT] ... : [haproxy.cfg:90] : ...

	// Find [filename:linenum] pattern - look for second [ (after [ALERT])
	_, after, ok := strings.Cut(alertLine, "[")
	if !ok {
		return ""
	}

	// Look for second bracket after [ALERT]
	remaining := after
	_, after, ok = strings.Cut(remaining, "[")
	if !ok {
		return ""
	}

	// Now parse the [filename:line] part
	fileLinePart := after
	colonIdx := strings.Index(fileLinePart, ":")
	if colonIdx == -1 {
		return ""
	}

	bracketClose := strings.Index(fileLinePart, "]")
	if bracketClose == -1 || bracketClose < colonIdx {
		return ""
	}

	// Extract line number part (after the colon, before the bracket)
	lineNumStr := fileLinePart[colonIdx+1 : bracketClose]
	lineNum := 0
	if _, err := fmt.Sscanf(lineNumStr, "%d", &lineNum); err != nil {
		return ""
	}

	// Convert to 0-based index
	errorLineIdx := lineNum - 1
	if errorLineIdx < 0 || errorLineIdx >= len(configLines) {
		return ""
	}

	// Calculate context range (3 lines before and after)
	startIdx := max(errorLineIdx-3, 0)

	endIdx := min(
		// +4 because we want 3 lines after
		errorLineIdx+4, len(configLines))

	// Build context block with line numbers
	var contextLines []string
	for i := startIdx; i < endIdx; i++ {
		lineContent := configLines[i]
		lineNumber := i + 1

		var formatted string
		if i == errorLineIdx {
			// Error line - add arrow marker
			formatted = fmt.Sprintf("  %4d → %s", lineNumber, lineContent)
		} else {
			formatted = fmt.Sprintf("  %4d   %s", lineNumber, lineContent)
		}

		// Trim trailing spaces for cleaner output
		contextLines = append(contextLines, strings.TrimRight(formatted, " "))
	}

	return strings.Join(contextLines, "\n")
}

// mapFileItems lists plain and sensitive maps alike: HAProxy reads both.
func mapFileItems(auxFiles *AuxiliaryFiles) []auxiliaryfiles.FileItem {
	items := make([]auxiliaryfiles.FileItem, 0, len(auxFiles.MapFiles)+len(auxFiles.SensitiveMapFiles))
	for _, file := range auxFiles.MapFiles {
		items = append(items, file)
	}
	for _, file := range auxFiles.SensitiveMapFiles {
		items = append(items, file)
	}
	return items
}
