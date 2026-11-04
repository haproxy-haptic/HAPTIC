// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"log/slog"
	"time"

	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

// NewBudgetedValidationTestsValidator exposes the execution budget to integration tests.
func NewBudgetedValidationTestsValidator(bus *busevents.EventBus, logger *slog.Logger, budget func(int) time.Duration) *ValidationTestsValidator {
	v := NewValidationTestsValidator(bus, logger, stubTypeBootstrapper)
	v.budgetFor = budget
	return v
}
