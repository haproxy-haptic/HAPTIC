package main

// configurationError distinguishes a rejected input from an unavailable check.
type configurationError struct{ err error }

func (e *configurationError) Error() string { return e.err.Error() }
func (e *configurationError) Unwrap() error { return e.err }
