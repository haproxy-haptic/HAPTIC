package incremental

import (
	"context"
	"errors"
	"fmt"
	"unique"
)

var (
	// ErrCommitConflict means another session committed first.
	ErrCommitConflict = errors.New("incremental graph commit conflict")
	// errInvalidBaseGeneration means the generation a session began on lost
	// its provenance, which no commit can cause.
	errInvalidBaseGeneration = errors.New("incremental graph base generation has invalid provenance")
	// ErrRevisionConflict means final input verification found a newer snapshot.
	ErrRevisionConflict = errors.New("incremental input revision conflict")
	// ErrVerifierRequired prevents publishing unverified speculative state.
	ErrVerifierRequired = errors.New("incremental input revision verifier is required")
	// ErrSessionClosed means a session is committing, committed, or aborted.
	ErrSessionClosed = errors.New("incremental graph session is closed")
	// ErrGenerationExhausted means no later commit generation can be represented.
	ErrGenerationExhausted = errors.New("incremental graph generation exhausted")
	// ErrResolverRequired prevents retiring inputs that cannot be loaded again.
	ErrResolverRequired = errors.New("incremental input resolver is required")
)

// Options controls graph-lifetime cache behavior.
type Options struct {
	RetireUnreferencedInputs bool
}

// InputKey is an opaque, comparable input identity.
//
// Keys are interned because a graph stores one per dependency edge, and edges
// outnumber distinct keys by orders of magnitude (#284).
type InputKey struct {
	id internedKey
}

// NewInputKey creates an input identity from caller-owned opaque data.
func NewInputKey(value string) InputKey {
	return InputKey{id: internKey(value)}
}

// Opaque returns the input identity without interpreting it.
func (k InputKey) Opaque() string {
	return k.id.String()
}

// QueryKey is an opaque, comparable query identity, interned like [InputKey].
type QueryKey struct {
	id internedKey
}

// NewQueryKey creates a query identity from caller-owned opaque data.
func NewQueryKey(value string) QueryKey {
	return QueryKey{id: internKey(value)}
}

// Opaque returns the query identity without interpreting it.
func (k QueryKey) Opaque() string {
	return k.id.String()
}

// internedKey is a canonical key string; the zero value is the empty key.
type internedKey struct {
	handle unique.Handle[string]
}

func internKey(value string) internedKey {
	if value == "" {
		return internedKey{}
	}
	return internedKey{handle: unique.Make(value)}
}

func (k internedKey) String() string {
	if k.handle == (unique.Handle[string]{}) {
		return ""
	}
	return k.handle.Value()
}

// Revision is an exact, comparable input revision token.
type Revision struct {
	value string
}

// NewRevision creates a revision from caller-owned opaque data.
func NewRevision(value string) Revision {
	return Revision{value: value}
}

// Opaque returns the exact caller-owned revision token.
func (r Revision) Opaque() string {
	return r.value
}

// Input binds immutable bytes, including an exact negative read, to a revision.
type Input struct {
	Key      InputKey
	Revision Revision
	Found    bool
	Value    []byte
}

// ImmutableInput binds arbitrary input bytes as an immutable string.
type ImmutableInput struct {
	Key      InputKey
	Revision Revision
	Found    bool
	Value    string
}

// InputRevision is an exact input observation passed to the commit verifier.
type InputRevision struct {
	Key      InputKey
	Revision Revision
	Found    bool
}

// Reader records dynamic query dependencies while a definition runs.
type Reader interface {
	Input(InputKey) ([]byte, bool, error)
	ExactInput(InputKey) (Input, error)
	Query(context.Context, QueryKey) ([]byte, error)
}

// OwnedInputReader transfers one detached input snapshot while recording its dependency.
type OwnedInputReader interface {
	Reader
	ExactInputOwned(InputKey) (Input, error)
}

// ExactInputObserver records an input dependency only when its exact identity
// still matches, without exposing or copying the input bytes.
type ExactInputObserver interface {
	Reader
	ObserveExactInput(InputRevision) error
	exactInputObserver()
}

// ExactInputValueObserver records an input only when its complete immutable value still matches.
type ExactInputValueObserver interface {
	Reader
	ObserveExactInputValue(Input) error
	exactInputValueObserver()
}

// ExactImmutableInputObserver records an input without exposing mutable bytes.
type ExactImmutableInputObserver interface {
	Reader
	ObserveExactImmutableInput(ImmutableInput) error
	exactImmutableInputObserver()
}

// ExactQueryObserver obtains one query value with an authenticated observation
// and lets sibling queries record that exact dependency without reading its bytes.
type ExactQueryObserver interface {
	Reader
	QueryWithExactObservation(context.Context, QueryKey) ([]byte, ExactQueryObservation, error)
	ObserveExactQuery(ExactQueryObservation) error
	exactQueryObserver()
}

// QueryFunc computes one immutable value. Its error is returned unwrapped, so it
// names the failing query in its own terms.
type QueryFunc func(context.Context, Reader) ([]byte, error)

// BatchQuery is one independently tracked query execution in a batch.
type BatchQuery struct {
	Key    QueryKey
	Reader Reader
	root   exactValueFactory
}

type exactValueFactory func(string) (ExactValueRoot, error)

// NewExactValue binds an immutable string to this live query execution.
func (q BatchQuery) NewExactValue(value string) (ExactValueRoot, error) {
	if q.root == nil {
		return ExactValueRoot{}, errors.New("incremental batch query has no exact-value authority")
	}
	return q.root(value)
}

// BatchValue is the value or error produced for one [BatchQuery].
type BatchValue struct {
	Value []byte
	Err   error
}

// BatchQueryFunc executes sorted queries without merging their dependency readers.
type BatchQueryFunc func(context.Context, []BatchQuery) ([]BatchValue, error)

// ExactBatchValue is the immutable value or error produced for one [BatchQuery].
type ExactBatchValue struct {
	Value ExactValueRoot
	Err   error
}

// ExactBatchQueryFunc executes sorted queries and returns query-bound immutable values.
type ExactBatchQueryFunc func(context.Context, []BatchQuery) ([]ExactBatchValue, error)

// DefinitionProvider resolves graph-lifetime dynamic query identities.
type DefinitionProvider func(QueryKey) (QueryFunc, bool)

// InputResolver atomically loads an exact snapshot for a previously unseen key.
type InputResolver func(context.Context, InputKey) (Input, error)

// Definition binds one stable query identity to its implementation.
type Definition struct {
	Key QueryKey
	Run QueryFunc
}

// RevisionVerifier confirms that every observation is still exact at commit.
type RevisionVerifier func(context.Context, []InputRevision) (bool, error)

// Result is one deterministically ordered query result.
type Result struct {
	Key   QueryKey
	Value []byte
}

// ExactResult is one deterministically ordered immutable query result.
type ExactResult struct {
	Key   QueryKey
	Value ExactValueRoot
}

// NodeCounters describe committed work for one query.
type NodeCounters struct {
	Executions    uint64
	CacheHits     uint64
	Backdates     uint64
	Changes       uint64
	Invalidations uint64
}

// CycleError identifies a dynamic dependency cycle.
type CycleError struct {
	Path []QueryKey
}

func (e *CycleError) Error() string {
	return "incremental query cycle"
}

type queryError struct {
	key QueryKey
	err error
}

func (e *queryError) Error() string {
	return fmt.Sprintf("incremental query %s failed: %v", e.key, e.err)
}

func (e *queryError) Unwrap() error {
	return e.err
}

type panicError struct {
	value any
}

func (e *panicError) Error() string {
	return fmt.Sprintf("incremental query panicked: %v", e.value)
}

type missingInputError struct {
	key InputKey
}

func (e *missingInputError) Error() string {
	return fmt.Sprintf("incremental query read input %s without an exact snapshot", e.key)
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func validInputKey(key InputKey) bool {
	return key.id.String() != ""
}

func validQueryKey(key QueryKey) bool {
	return key.id.String() != ""
}

func validRevision(revision Revision) bool {
	return revision.value != ""
}
