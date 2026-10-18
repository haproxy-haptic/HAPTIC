package orderedset

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Authority owns one isolated family of immutable roots.
type Authority struct {
	seal  *Authority
	empty *rootState
}

// Scope binds a non-empty root to one exact caller-owned identity.
type Scope struct {
	Domain uint8
	Key    string
}

type rootState struct {
	authority *Authority
	scope     Scope
	node      *node
	packed    []string
	size      int
	seal      *rootState
}

type node struct {
	authority *Authority
	key       string
	left      *node
	right     *node
	height    int
	size      int
	seal      *node
}

// Root is an opaque immutable ordered-set root.
type Root struct {
	state *rootState
}

// NewAuthority creates an isolated root authority.
func NewAuthority() *Authority {
	authority := &Authority{}
	authority.seal = authority
	authority.empty = newRootState(authority, Scope{}, nil)
	return authority
}

// Empty returns the authority's canonical empty root.
func (a *Authority) Empty() Root {
	if !a.valid() {
		return Root{}
	}
	return Root{state: a.empty}
}

// BuildSorted creates an authenticated root from strictly increasing values.
func BuildSorted(authority *Authority, scope Scope, values []string) (Root, error) {
	if err := validateSortedBuild(authority, scope, values); err != nil {
		return Root{}, err
	}
	if len(values) == 0 {
		return authority.Empty(), nil
	}
	return Root{state: newRootState(authority, scope, buildSortedNode(authority, values))}, nil
}

// BuildPackedSorted creates an authenticated compact root from strictly increasing values.
func BuildPackedSorted(authority *Authority, scope Scope, values []string) (Root, error) {
	if err := validateSortedBuild(authority, scope, values); err != nil {
		return Root{}, err
	}
	if len(values) == 0 {
		return authority.Empty(), nil
	}
	return Root{state: newPackedRootState(authority, scope, slices.Clone(values))}, nil
}

func validateSortedBuild(authority *Authority, scope Scope, values []string) error {
	if !authority.valid() {
		return errors.New("ordered set has invalid provenance")
	}
	if !scope.valid() {
		return errors.New("ordered set belongs to another scope")
	}
	for index, value := range values {
		if value == "" {
			return errors.New("ordered set value is empty")
		}
		if index != 0 && values[index-1] >= value {
			return errors.New("ordered set values are not strictly increasing")
		}
	}
	return nil
}

// ValidateAuthentication verifies root ownership and its immutable representation in O(1).
func (r Root) ValidateAuthentication(authority *Authority) error {
	if !r.authenticState(authority) {
		return errors.New("ordered set has invalid provenance")
	}
	if r.state.size == 0 {
		if !r.emptyStateAuthentic(authority) {
			return errors.New("ordered set has invalid provenance")
		}
		return nil
	}
	if !r.populatedStateAuthentic() {
		return errors.New("ordered set has invalid provenance")
	}
	if r.state.node != nil {
		return validateNode(authority, r.state.node)
	}
	return nil
}

func (r Root) authenticState(authority *Authority) bool {
	return authority.valid() && r.state != nil && r.state.seal == r.state &&
		r.state.authority == authority && r.state.size >= 0 &&
		(r.state.node == nil || len(r.state.packed) == 0)
}

func (r Root) emptyStateAuthentic(authority *Authority) bool {
	return r.state == authority.empty && r.state.scope == (Scope{}) &&
		r.state.node == nil && len(r.state.packed) == 0
}

func (r Root) populatedStateAuthentic() bool {
	if !r.state.scope.valid() {
		return false
	}
	if r.state.node == nil {
		return len(r.state.packed) == r.state.size && len(r.state.packed) != 0
	}
	return r.state.size == nodeSize(r.state.node)
}

// ValidateOwnership verifies provenance and exact scope ownership in O(1).
func (r Root) ValidateOwnership(authority *Authority, scope Scope) error {
	if err := r.ValidateAuthentication(authority); err != nil {
		return err
	}
	if !scope.valid() || r.state.size != 0 && r.state.scope != scope {
		return errors.New("ordered set belongs to another scope")
	}
	return nil
}

// Len returns the authenticated element count.
func (r Root) Len(authority *Authority, scope Scope) (int, error) {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return 0, err
	}
	return r.state.size, nil
}

// Contains reports whether value is present.
func (r Root) Contains(authority *Authority, scope Scope, value string) (bool, error) {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return false, err
	}
	if len(r.state.packed) != 0 {
		_, found := slices.BinarySearch(r.state.packed, value)
		return found, nil
	}
	current := r.state.node
	for current != nil {
		if err := validateNode(authority, current); err != nil {
			return false, err
		}
		switch strings.Compare(value, current.key) {
		case -1:
			current = current.left
		case 1:
			current = current.right
		default:
			return true, nil
		}
	}
	return false, nil
}

// Add returns a root containing value and whether the set changed.
func (r Root) Add(authority *Authority, scope Scope, value string) (Root, bool, error) {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return Root{}, false, err
	}
	if value == "" {
		return Root{}, false, errors.New("ordered set value is empty")
	}
	if len(r.state.packed) != 0 {
		index, found := slices.BinarySearch(r.state.packed, value)
		if found {
			return r, false, nil
		}
		values := make([]string, len(r.state.packed)+1)
		copy(values, r.state.packed[:index])
		values[index] = value
		copy(values[index+1:], r.state.packed[index:])
		return Root{state: newPackedRootState(authority, scope, values)}, true, nil
	}
	next, changed, err := addNode(authority, r.state.node, value)
	if err != nil {
		return Root{}, false, err
	}
	if !changed {
		return r, false, nil
	}
	return Root{state: newRootState(authority, scope, next)}, true, nil
}

// Delete returns a root without value and whether the set changed.
func (r Root) Delete(authority *Authority, scope Scope, value string) (Root, bool, error) {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return Root{}, false, err
	}
	if len(r.state.packed) != 0 {
		index, found := slices.BinarySearch(r.state.packed, value)
		if !found {
			return r, false, nil
		}
		if len(r.state.packed) == 1 {
			return authority.Empty(), true, nil
		}
		values := make([]string, len(r.state.packed)-1)
		copy(values, r.state.packed[:index])
		copy(values[index:], r.state.packed[index+1:])
		return Root{state: newPackedRootState(authority, scope, values)}, true, nil
	}
	next, changed, err := deleteNode(authority, r.state.node, value)
	if err != nil {
		return Root{}, false, err
	}
	if !changed {
		return r, false, nil
	}
	if next == nil {
		return authority.Empty(), true, nil
	}
	return Root{state: newRootState(authority, scope, next)}, true, nil
}

// Range visits values in ascending order until visit returns false.
func (r Root) Range(authority *Authority, scope Scope, visit func(string) bool) error {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return err
	}
	if len(r.state.packed) != 0 {
		for _, value := range r.state.packed {
			if !visit(value) {
				break
			}
		}
		return nil
	}
	_, err := rangeNode(authority, r.state.node, nil, nil, visit)
	return err
}

// Values returns a detached sorted projection.
func (r Root) Values(authority *Authority, scope Scope) ([]string, error) {
	size, err := r.Len(authority, scope)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, size)
	err = r.Range(authority, scope, func(value string) bool {
		values = append(values, value)
		return true
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// SameRoot reports authenticated process-local root identity.
func (r Root) SameRoot(authority *Authority, scope Scope, other Root) (bool, error) {
	if err := r.ValidateOwnership(authority, scope); err != nil {
		return false, err
	}
	if err := other.ValidateOwnership(authority, scope); err != nil {
		return false, err
	}
	return r.state == other.state, nil
}

func (a *Authority) valid() bool {
	return a != nil && a.seal == a && a.empty != nil && a.empty.seal == a.empty &&
		a.empty.authority == a && a.empty.scope == (Scope{}) && a.empty.node == nil &&
		len(a.empty.packed) == 0 && a.empty.size == 0
}

func (s Scope) valid() bool {
	return s.Domain != 0 && s.Key != ""
}

func newRootState(authority *Authority, scope Scope, root *node) *rootState {
	state := &rootState{authority: authority, scope: scope, node: root, size: nodeSize(root)}
	state.seal = state
	return state
}

func newPackedRootState(authority *Authority, scope Scope, values []string) *rootState {
	state := &rootState{authority: authority, scope: scope, packed: values, size: len(values)}
	state.seal = state
	return state
}

func newNode(authority *Authority, key string, left, right *node) *node {
	created := &node{
		authority: authority,
		key:       key,
		left:      left,
		right:     right,
		height:    1 + max(nodeHeight(left), nodeHeight(right)),
		size:      1 + nodeSize(left) + nodeSize(right),
	}
	created.seal = created
	return created
}

func buildSortedNode(authority *Authority, values []string) *node {
	if len(values) == 0 {
		return nil
	}
	middle := len(values) / 2
	return newNode(
		authority,
		values[middle],
		buildSortedNode(authority, values[:middle]),
		buildSortedNode(authority, values[middle+1:]),
	)
}

func validateNode(authority *Authority, current *node) error {
	if err := validateNodeShape(authority, current); err != nil {
		return err
	}
	if current != nil && (balanceFactor(current) < -1 || balanceFactor(current) > 1) {
		return errors.New("ordered set is not balanced")
	}
	return nil
}

func validateNodeShape(authority *Authority, current *node) error {
	if current == nil {
		return nil
	}
	if current.seal != current || current.authority != authority {
		return errors.New("ordered set has invalid provenance")
	}
	if current.height != 1+max(nodeHeight(current.left), nodeHeight(current.right)) {
		return errors.New("ordered set has an invalid height")
	}
	if current.size != 1+nodeSize(current.left)+nodeSize(current.right) {
		return errors.New("ordered set has an invalid size")
	}
	if current.left != nil && (current.left.seal != current.left || current.left.authority != authority) {
		return errors.New("ordered set has invalid provenance")
	}
	if current.right != nil && (current.right.seal != current.right || current.right.authority != authority) {
		return errors.New("ordered set has invalid provenance")
	}
	return nil
}

func addNode(authority *Authority, current *node, value string) (*node, bool, error) {
	if current == nil {
		return newNode(authority, value, nil, nil), true, nil
	}
	if err := validateNode(authority, current); err != nil {
		return nil, false, fmt.Errorf("validating node before adding a value: %w", err)
	}
	switch strings.Compare(value, current.key) {
	case -1:
		left, changed, err := addNode(authority, current.left, value)
		if err != nil || !changed {
			return current, changed, err
		}
		return rebalance(authority, newNode(authority, current.key, left, current.right))
	case 1:
		right, changed, err := addNode(authority, current.right, value)
		if err != nil || !changed {
			return current, changed, err
		}
		return rebalance(authority, newNode(authority, current.key, current.left, right))
	default:
		return current, false, nil
	}
}

func deleteNode(authority *Authority, current *node, value string) (*node, bool, error) {
	if current == nil {
		return nil, false, nil
	}
	if err := validateNode(authority, current); err != nil {
		return nil, false, err
	}
	switch strings.Compare(value, current.key) {
	case -1:
		left, changed, err := deleteNode(authority, current.left, value)
		if err != nil || !changed {
			return current, changed, err
		}
		return rebalance(authority, newNode(authority, current.key, left, current.right))
	case 1:
		right, changed, err := deleteNode(authority, current.right, value)
		if err != nil || !changed {
			return current, changed, err
		}
		return rebalance(authority, newNode(authority, current.key, current.left, right))
	default:
		if current.left == nil {
			return current.right, true, nil
		}
		if current.right == nil {
			return current.left, true, nil
		}
		successor, err := minimumNode(authority, current.right)
		if err != nil {
			return nil, false, err
		}
		right, _, err := deleteNode(authority, current.right, successor.key)
		if err != nil {
			return nil, false, err
		}
		return rebalance(authority, newNode(authority, successor.key, current.left, right))
	}
}

func minimumNode(authority *Authority, current *node) (*node, error) {
	for {
		if err := validateNode(authority, current); err != nil {
			return nil, err
		}
		if current.left == nil {
			return current, nil
		}
		current = current.left
	}
}

func rebalance(authority *Authority, current *node) (*node, bool, error) {
	switch balanceFactor(current) {
	case 2:
		left := current.left
		if err := validateNode(authority, left); err != nil {
			return nil, false, fmt.Errorf("rebalancing left child: %w", err)
		}
		if balanceFactor(left) < 0 {
			rotated, err := rotateLeft(authority, left)
			if err != nil {
				return nil, false, err
			}
			current = newNode(authority, current.key, rotated, current.right)
		}
		rotated, err := rotateRight(authority, current)
		if err != nil {
			return nil, false, fmt.Errorf("rotating right: %w", err)
		}
		return rotated, true, nil
	case -2:
		right := current.right
		if err := validateNode(authority, right); err != nil {
			return nil, false, fmt.Errorf("rebalancing right child: %w", err)
		}
		if balanceFactor(right) > 0 {
			rotated, err := rotateRight(authority, right)
			if err != nil {
				return nil, false, err
			}
			current = newNode(authority, current.key, current.left, rotated)
		}
		rotated, err := rotateLeft(authority, current)
		if err != nil {
			return nil, false, fmt.Errorf("rotating left: %w", err)
		}
		return rotated, true, nil
	default:
		return current, true, nil
	}
}

func rotateLeft(authority *Authority, current *node) (*node, error) {
	right := current.right
	if err := validateNodeShape(authority, right); err != nil {
		return nil, err
	}
	left := newNode(authority, current.key, current.left, right.left)
	return newNode(authority, right.key, left, right.right), nil
}

func rotateRight(authority *Authority, current *node) (*node, error) {
	left := current.left
	if err := validateNodeShape(authority, left); err != nil {
		return nil, err
	}
	right := newNode(authority, current.key, left.right, current.right)
	return newNode(authority, left.key, left.left, right), nil
}

func rangeNode(
	authority *Authority,
	current *node,
	minimum *string,
	maximum *string,
	visit func(string) bool,
) (bool, error) {
	if current == nil {
		return true, nil
	}
	if err := validateNode(authority, current); err != nil {
		return false, err
	}
	if minimum != nil && current.key <= *minimum || maximum != nil && current.key >= *maximum {
		return false, errors.New("ordered set has invalid provenance")
	}
	if proceed, err := rangeNode(authority, current.left, minimum, &current.key, visit); err != nil || !proceed {
		return proceed, err
	}
	if !visit(current.key) {
		return false, nil
	}
	return rangeNode(authority, current.right, &current.key, maximum, visit)
}

func nodeHeight(current *node) int {
	if current == nil {
		return 0
	}
	return current.height
}

func nodeSize(current *node) int {
	if current == nil {
		return 0
	}
	return current.size
}

func balanceFactor(current *node) int {
	return nodeHeight(current.left) - nodeHeight(current.right)
}
