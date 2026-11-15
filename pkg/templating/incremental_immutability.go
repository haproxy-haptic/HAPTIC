// Copyright 2026 Philipp Hossner
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

package templating

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/builtin"
	"github.com/haproxy-haptic/scriggo/native"
)

type immutableStorageContextKey struct{}

const (
	immutableStorageTemplateContextKey            = "__haptic_immutable_resource_inputs"
	incrementalImmutableBindingTemplateContextKey = "__haptic_incremental_immutable_inputs"
)

// memberProfile joins the plan-registrar member names in declaration_names.go.
const memberProfile = "Profile"

var (
	resourceStoreType                   = reflect.TypeFor[ResourceStore]()
	mapStringAnyType                    = reflect.TypeFor[map[string]any]()
	sliceAnyType                        = reflect.TypeFor[[]any]()
	httpFetcherType                     = reflect.TypeFor[HTTPFetcher]()
	fileRegistrarType                   = reflect.TypeFor[FileRegistrar]()
	planRegistrarType                   = reflect.TypeFor[PlanRegistrar]()
	incrementalBackendPlanRegistrarType = reflect.TypeFor[IncrementalBackendPlanRegistrar]()
	builtinRegexpType                   = reflect.TypeFor[builtin.Regexp]()
)

type immutableIdentity struct {
	kind reflect.Kind
	ptr  uintptr
}

type immutableTarget struct {
	identity immutableIdentity
	pointer  uintptr
}

type immutableVisit struct {
	identity immutableIdentity
	length   int
	capacity int
}

type immutableVisitSet struct {
	small  [8]immutableVisit
	count  int
	values map[immutableVisit]struct{}
}

func (s *immutableVisitSet) add(visit immutableVisit) bool {
	if s.values != nil {
		if _, exists := s.values[visit]; exists {
			return false
		}
		s.values[visit] = struct{}{}
		return true
	}
	for index := range s.count {
		if s.small[index] == visit {
			return false
		}
	}
	if s.count < len(s.small) {
		s.small[s.count] = visit
		s.count++
		return true
	}
	s.values = make(map[immutableVisit]struct{}, len(s.small)*2)
	for _, existing := range s.small {
		s.values[existing] = struct{}{}
	}
	s.values[visit] = struct{}{}
	s.small = [8]immutableVisit{}
	s.count = 0
	return true
}

type immutableRange struct {
	start uintptr
	end   uintptr
}

type immutableRangeNode struct {
	rangeValue immutableRange
	maxEnd     uintptr
	height     int
	left       *immutableRangeNode
	right      *immutableRangeNode
}

type immutableRangeIndex struct {
	small [4]immutableRange
	count int
	root  *immutableRangeNode
}

type immutableStorage struct {
	mu             sync.RWMutex
	parent         *immutableStorage
	certified      *immutableCertificateIndex
	certifiedSmall [8]*incrementalImmutableCertificateView
	certifiedCount int
	identities     map[immutableIdentity]struct{}
	identitySmall  [8]immutableIdentity
	identityCount  int
	ranges         immutableRangeIndex
	keep           []any
}

type incrementalImmutableBinding struct {
	seal          *incrementalImmutableBinding
	storage       *immutableStorage
	source        string
	item          immutableIdentity
	props         immutableIdentity
	renderSubject immutableIdentity
	controller    immutableIdentity
	resources     immutableIdentity
	hasResources  bool
}

// IncrementalImmutableCertificate is a reusable immutable-value guard.
type IncrementalImmutableCertificate struct {
	seal  *IncrementalImmutableCertificate
	proof *incrementalImmutableCertificateProof
	view  *incrementalImmutableCertificateView
}

type incrementalImmutableCertificateView struct {
	seal          *incrementalImmutableCertificateView
	proof         *incrementalImmutableCertificateViewProof
	certificate   *IncrementalImmutableCertificate
	identitySlots []immutableIdentity
	identityIndex bool
	ranges        []immutableRange
	keep          []any
}

type incrementalImmutableCertificateProof struct {
	seal        *incrementalImmutableCertificateProof
	certificate *IncrementalImmutableCertificate
	view        *incrementalImmutableCertificateView
}

type incrementalImmutableCertificateViewProof struct {
	seal                    *incrementalImmutableCertificateViewProof
	view                    *incrementalImmutableCertificateView
	certificate             *IncrementalImmutableCertificate
	identitySlots           immutableCertificateSliceIdentity
	identityIndex           bool
	ranges                  immutableCertificateSliceIdentity
	keep                    immutableCertificateSliceIdentity
	registeredIdentitySlots []immutableIdentity
	registeredRanges        []immutableRange
	retained                []any
	retainedSlice           immutableCertificateSliceIdentity
	anchors                 []immutableCertificateAnchor
}

type immutableCertificateSliceIdentity struct {
	pointer  uintptr
	length   int
	capacity int
	isNil    bool
}

type immutableCertificateAnchor struct {
	typeOf   reflect.Type
	kind     reflect.Kind
	pointer  uintptr
	length   int
	capacity int
	isNil    bool
	value    any
}

// IncrementalCertifiedValues carries reusable immutable values to the template runtime.
type IncrementalCertifiedValues struct {
	values      []any
	certificate *IncrementalImmutableCertificate
	seal        *IncrementalCertifiedValues
}

// NewIncrementalCertifiedValues seals values for the template runtime.
func NewIncrementalCertifiedValues(
	values []any,
	certificate *IncrementalImmutableCertificate,
) *IncrementalCertifiedValues {
	if certificate == nil || !certificate.Guards(values) {
		return nil
	}
	certified := &IncrementalCertifiedValues{values: values, certificate: certificate}
	certified.seal = certified
	return certified
}

func (v *IncrementalCertifiedValues) unwrap() ([]any, *IncrementalImmutableCertificate, bool) {
	if v == nil || v.seal != v || v.certificate == nil || !v.certificate.Guards(v.values) {
		return nil, nil, false
	}
	return v.values, v.certificate, true
}

// ValidateAuthentication verifies the immutable value view without walking it.
func (v *IncrementalCertifiedValues) ValidateAuthentication() error {
	if _, _, valid := v.unwrap(); !valid {
		return errors.New("incremental certified values have invalid provenance")
	}
	return nil
}

// SameRoot reports whether two certified views reference the same immutable values.
func (v *IncrementalCertifiedValues) SameRoot(other *IncrementalCertifiedValues) (bool, error) {
	if err := v.ValidateAuthentication(); err != nil {
		return false, err
	}
	if err := other.ValidateAuthentication(); err != nil {
		return false, err
	}
	return v.certificate == other.certificate && immutableCertificateSlice(v.values) ==
		immutableCertificateSlice(other.values), nil
}

// CertifyIncrementalImmutableInputs prepares a reusable guard for immutable values.
func CertifyIncrementalImmutableInputs(values ...any) *IncrementalImmutableCertificate {
	builder := immutableCertificateBuilder{}
	for _, value := range values {
		builder.collectAny(value)
	}
	return builder.certificate(values)
}

// Guards reports whether the certificate protects the exact referenced value.
func (c *IncrementalImmutableCertificate) Guards(value any) bool {
	if !c.authenticate() {
		return false
	}
	for _, anchor := range c.view.keep {
		if sameImmutableCertificateAnchor(anchor, value) {
			return true
		}
	}
	return false
}

func (c *IncrementalImmutableCertificate) authenticate() bool {
	if c == nil || c.seal != c || c.proof == nil || c.proof.seal != c.proof ||
		c.proof.certificate != c || c.view == nil || c.proof.view != c.view {
		return false
	}
	return c.view.authenticate()
}

func (v *incrementalImmutableCertificateView) authenticate() bool {
	if !v.authenticateRegistered() || v.identityIndex != v.proof.identityIndex ||
		immutableCertificateSlice(v.identitySlots) != v.proof.identitySlots ||
		!slices.Equal(v.identitySlots, v.proof.registeredIdentitySlots) ||
		immutableCertificateSlice(v.ranges) != v.proof.ranges ||
		!slices.Equal(v.ranges, v.proof.registeredRanges) ||
		immutableCertificateSlice(v.keep) != v.proof.keep ||
		immutableCertificateSlice(v.proof.retained) != v.proof.retainedSlice ||
		len(v.keep) != len(v.proof.anchors) {
		return false
	}
	for index := range v.keep {
		if !v.proof.anchors[index].matches(v.keep[index]) {
			return false
		}
	}
	return true
}

func (v *incrementalImmutableCertificateView) authenticateRegistered() bool {
	if v == nil || v.seal != v || v.proof == nil || v.proof.seal != v.proof ||
		v.proof.view != v || v.certificate == nil || v.proof.certificate != v.certificate ||
		v.certificate.seal != v.certificate || v.certificate.proof == nil ||
		v.certificate.proof.seal != v.certificate.proof ||
		v.certificate.proof.certificate != v.certificate ||
		v.certificate.view != v || v.certificate.proof.view != v {
		return false
	}
	return true
}

func immutableCertificateSlice[T any](values []T) immutableCertificateSliceIdentity {
	var pointer uintptr
	if cap(values) > 0 {
		// Via the element address, not the slice: a slice is not pointer-shaped,
		// so reflect.ValueOf(values) boxes its header — one allocation per call,
		// on a function the immutability guards call dozens of times per render.
		pointer = reflect.ValueOf(&values[:1][0]).Pointer()
	}
	return immutableCertificateSliceIdentity{
		pointer:  pointer,
		length:   len(values),
		capacity: cap(values),
		isNil:    values == nil,
	}
}

func newImmutableCertificateAnchor(value any) immutableCertificateAnchor {
	reflected := immutableConcreteValue(reflect.ValueOf(value))
	if !reflected.IsValid() {
		return immutableCertificateAnchor{kind: reflect.Invalid}
	}
	anchor := immutableCertificateAnchor{typeOf: reflected.Type(), kind: reflected.Kind()}
	switch reflected.Kind() {
	case reflect.Map, reflect.Pointer:
		anchor.isNil = reflected.IsNil()
		if !anchor.isNil {
			anchor.pointer = reflected.Pointer()
		}
	case reflect.Slice:
		anchor.isNil = reflected.IsNil()
		anchor.length = reflected.Len()
		anchor.capacity = reflected.Cap()
		if !anchor.isNil && anchor.capacity != 0 {
			anchor.pointer = reflected.Pointer()
		}
	default:
		anchor.value = value
	}
	return anchor
}

func (a immutableCertificateAnchor) matches(value any) bool {
	reflected := immutableConcreteValue(reflect.ValueOf(value))
	if !reflected.IsValid() {
		return a.kind == reflect.Invalid
	}
	if reflected.Type() != a.typeOf || reflected.Kind() != a.kind {
		return false
	}
	switch reflected.Kind() {
	case reflect.Map, reflect.Pointer:
		return reflected.IsNil() == a.isNil && (a.isNil || reflected.Pointer() == a.pointer)
	case reflect.Slice:
		return reflected.IsNil() == a.isNil && reflected.Len() == a.length &&
			reflected.Cap() == a.capacity && (a.isNil || a.capacity == 0 || reflected.Pointer() == a.pointer)
	default:
		return reflect.DeepEqual(a.value, value)
	}
}

func sameImmutableCertificateAnchor(left, right any) bool {
	leftValue := immutableConcreteValue(reflect.ValueOf(left))
	rightValue := immutableConcreteValue(reflect.ValueOf(right))
	if !leftValue.IsValid() || !rightValue.IsValid() || leftValue.Type() != rightValue.Type() {
		return !leftValue.IsValid() && !rightValue.IsValid()
	}
	switch leftValue.Kind() {
	case reflect.Map, reflect.Pointer:
		return leftValue.IsNil() == rightValue.IsNil() &&
			(leftValue.IsNil() || leftValue.Pointer() == rightValue.Pointer())
	case reflect.Slice:
		return leftValue.IsNil() == rightValue.IsNil() && leftValue.Len() == rightValue.Len() &&
			leftValue.Cap() == rightValue.Cap() &&
			(leftValue.IsNil() || leftValue.Cap() == 0 || leftValue.Pointer() == rightValue.Pointer())
	default:
		return reflect.DeepEqual(left, right)
	}
}

// WithIncrementalImmutableCertificates attaches reusable guards without walking their values again.
func WithIncrementalImmutableCertificates(
	ctx context.Context,
	certificates ...*IncrementalImmutableCertificate,
) context.Context {
	parent, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	storage := &immutableStorage{parent: parent}
	storage.addCertificates(certificates...)
	return context.WithValue(ctx, immutableStorageContextKey{}, storage)
}

// WithIncrementalImmutableInputs guards graph values from template mutation.
func WithIncrementalImmutableInputs(ctx context.Context, values ...any) context.Context {
	parent, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	storage := &immutableStorage{parent: parent}
	storage.add(values...)
	return context.WithValue(ctx, immutableStorageContextKey{}, storage)
}

// WithIncrementalImmutableCapabilityInputs guards a reflected capability
// surface without traversing function closures or adapter internals. Values
// returned through the capability must be registered separately.
func WithIncrementalImmutableCapabilityInputs(ctx context.Context, values ...any) context.Context {
	parent, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	storage := &immutableStorage{parent: parent}
	storage.addCapabilities(values...)
	return context.WithValue(ctx, immutableStorageContextKey{}, storage)
}

// WithImmutableResourceInputs guards resource values returned during one render.
func WithImmutableResourceInputs(ctx context.Context) context.Context {
	storage := &immutableStorage{}
	return context.WithValue(ctx, immutableStorageContextKey{}, storage)
}

// BindImmutableResourceInputs makes a prepared guard available to template execution.
func BindImmutableResourceInputs(templateContext map[string]any, ctx context.Context) error {
	if templateContext == nil || ctx == nil {
		return errors.New("immutable resource input binding is unavailable")
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage == nil {
		return errors.New("immutable resource input storage is unavailable")
	}
	templateContext[immutableStorageTemplateContextKey] = storage
	return nil
}

// WithBoundImmutableResourceInputs attaches a render context's prepared guard.
func WithBoundImmutableResourceInputs(
	ctx context.Context,
	templateContext map[string]any,
) (context.Context, error) {
	if templateContext == nil || ctx == nil {
		return nil, errors.New("immutable resource input binding is unavailable")
	}
	storage, _ := templateContext[immutableStorageTemplateContextKey].(*immutableStorage)
	if storage == nil {
		return nil, errors.New("immutable resource input storage is unavailable")
	}
	if current, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage); current == storage {
		return ctx, nil
	}
	return context.WithValue(ctx, immutableStorageContextKey{}, storage), nil
}

// withRenderImmutableResourceInputs binds the prepared guard a render context
// carries, preparing and binding one over ctx when it carries none, so the
// storage a render enforces is always the storage its inputs register into.
func withRenderImmutableResourceInputs(
	ctx context.Context,
	templateContext map[string]any,
) (context.Context, error) {
	if templateContext == nil || ctx == nil {
		return nil, errors.New("immutable resource input binding is unavailable")
	}
	if _, prepared := templateContext[immutableStorageTemplateContextKey]; prepared {
		return WithBoundImmutableResourceInputs(ctx, templateContext)
	}
	guarded := WithIncrementalImmutableInputs(ctx)
	if err := BindImmutableResourceInputs(templateContext, guarded); err != nil {
		return nil, err
	}
	return guarded, nil
}

// BindIncrementalImmutableInputs authenticates a prepared component's immutable guard.
func BindIncrementalImmutableInputs(templateContext map[string]any, ctx context.Context) error {
	if templateContext == nil {
		return errors.New("incremental immutable input binding is unavailable")
	}
	storage, err := incrementalImmutableStorage(ctx)
	if err != nil {
		return err
	}
	return bindIncrementalImmutableInputs(templateContext, storage)
}

func bindIncrementalImmutableInputs(
	templateContext map[string]any,
	storage *immutableStorage,
) error {
	if templateContext == nil || storage == nil {
		return errors.New("incremental immutable input binding is unavailable")
	}
	if _, exists := templateContext[incrementalImmutableBindingTemplateContextKey]; exists {
		return errors.New("incremental immutable input binding is already present")
	}
	source, ok := templateContext[declSource].(string)
	if !ok || source == "" {
		return errors.New("incremental immutable input binding requires a non-empty source")
	}
	item, err := boundIncrementalImmutableIdentity(storage, templateContext, declItem)
	if err != nil {
		return err
	}
	props, err := boundIncrementalImmutableIdentity(storage, templateContext, declProps)
	if err != nil {
		return err
	}
	renderSubject, err := boundIncrementalImmutableIdentity(storage, templateContext, declRenderSubject)
	if err != nil {
		return err
	}
	controller, err := boundIncrementalImmutableIdentity(storage, templateContext, declController)
	if err != nil {
		return err
	}
	binding := &incrementalImmutableBinding{
		storage: storage, source: source, item: item, props: props,
		renderSubject: renderSubject, controller: controller,
	}
	if _, exists := templateContext[declResources]; exists {
		binding.resources, err = boundIncrementalImmutableIdentity(storage, templateContext, declResources)
		if err != nil {
			return err
		}
		binding.hasResources = true
	}
	binding.seal = binding
	templateContext[incrementalImmutableBindingTemplateContextKey] = binding
	return nil
}

func newIncrementalImmutableValueBinding(
	storage *immutableStorage,
	values *IncrementalComponentContextValues,
) (incrementalImmutableBinding, error) {
	if storage == nil || values.Source == "" {
		return incrementalImmutableBinding{}, errors.New(
			"incremental immutable input binding requires a non-empty source",
		)
	}
	mode, ok := values.RenderSubject["mode"].(string)
	if !ok || (mode != renderModeReconcile && mode != renderModeAdmission) || values.RenderMode != mode {
		return incrementalImmutableBinding{}, errors.New("incremental immutable input binding has an invalid render mode")
	}
	item, err := boundIncrementalImmutableValueIdentity(storage, declItem, values.Item)
	if err != nil {
		return incrementalImmutableBinding{}, err
	}
	props, err := boundIncrementalImmutableValueIdentity(storage, declProps, values.Props)
	if err != nil {
		return incrementalImmutableBinding{}, err
	}
	renderSubject, err := boundIncrementalImmutableValueIdentity(storage, declRenderSubject, values.RenderSubject)
	if err != nil {
		return incrementalImmutableBinding{}, err
	}
	controller, err := boundIncrementalImmutableValueIdentity(storage, declController, values.Controller)
	if err != nil {
		return incrementalImmutableBinding{}, err
	}
	resources, err := boundIncrementalImmutableValueIdentity(storage, declResources, values.Resources)
	if err != nil {
		return incrementalImmutableBinding{}, err
	}
	return incrementalImmutableBinding{
		storage: storage, source: values.Source, item: item, props: props,
		renderSubject: renderSubject, controller: controller, resources: resources,
		hasResources: true,
	}, nil
}

func withBoundIncrementalImmutableInputs(
	ctx context.Context,
	templateContext map[string]any,
	inputs []any,
) (context.Context, error) {
	value, exists := templateContext[incrementalImmutableBindingTemplateContextKey]
	if !exists {
		return WithIncrementalImmutableInputs(ctx, inputs...), nil
	}
	binding, ok := value.(*incrementalImmutableBinding)
	if !ok || binding == nil || binding.seal != binding || binding.storage == nil {
		return nil, errors.New("incremental immutable input binding has invalid provenance")
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage != binding.storage {
		return nil, errors.New("incremental immutable input binding does not match its storage")
	}
	if err := binding.matches(templateContext); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (b *incrementalImmutableBinding) matches(templateContext map[string]any) error {
	source, ok := templateContext[declSource].(string)
	if !ok || source != b.source {
		return errors.New("incremental immutable input binding does not match source")
	}
	checks := []struct {
		name     string
		identity immutableIdentity
	}{
		{name: declItem, identity: b.item},
		{name: declProps, identity: b.props},
		{name: declRenderSubject, identity: b.renderSubject},
		{name: declController, identity: b.controller},
	}
	if b.hasResources {
		checks = append(checks, struct {
			name     string
			identity immutableIdentity
		}{name: declResources, identity: b.resources})
	} else if _, exists := templateContext[declResources]; exists {
		return errors.New("incremental immutable input binding does not match resources")
	}
	for _, check := range checks {
		value, exists := templateContext[check.name]
		if !exists || immutableBindingIdentity(reflect.ValueOf(value)) != check.identity ||
			!b.storage.contains(reflect.ValueOf(value)) {
			return fmt.Errorf("incremental immutable input binding does not match %s", check.name)
		}
	}
	return nil
}

func (b *incrementalImmutableBinding) matchesValues(values *IncrementalComponentContextValues) error {
	if b == nil || b.seal != b || b.storage == nil || values.Source != b.source {
		return errors.New("incremental immutable input binding does not match source")
	}
	checks := []struct {
		name     string
		value    any
		identity immutableIdentity
	}{
		{name: declItem, value: values.Item, identity: b.item},
		{name: declProps, value: values.Props, identity: b.props},
		{name: declRenderSubject, value: values.RenderSubject, identity: b.renderSubject},
		{name: declController, value: values.Controller, identity: b.controller},
		{name: declResources, value: values.Resources, identity: b.resources},
	}
	if !b.hasResources {
		return errors.New("incremental immutable input binding does not match resources")
	}
	for _, check := range checks {
		reflected := reflect.ValueOf(check.value)
		if immutableBindingIdentity(reflected) != check.identity || !b.storage.contains(reflected) {
			return fmt.Errorf("incremental immutable input binding does not match %s", check.name)
		}
	}
	return nil
}

func boundIncrementalImmutableIdentity(
	storage *immutableStorage,
	templateContext map[string]any,
	name string,
) (immutableIdentity, error) {
	value, exists := templateContext[name]
	if !exists {
		return immutableIdentity{}, fmt.Errorf("incremental immutable input binding requires %s", name)
	}
	return boundIncrementalImmutableValueIdentity(storage, name, value)
}

func boundIncrementalImmutableValueIdentity(
	storage *immutableStorage,
	name string,
	value any,
) (immutableIdentity, error) {
	reflected := reflect.ValueOf(value)
	identity := immutableBindingIdentity(reflected)
	if identity.kind == reflect.Invalid || !storage.contains(reflected) {
		return immutableIdentity{}, fmt.Errorf("incremental immutable input binding has no guard for %s", name)
	}
	return identity, nil
}

func immutableBindingIdentity(value reflect.Value) immutableIdentity {
	value = immutableConcreteValue(value)
	if !value.IsValid() {
		return immutableIdentity{}
	}
	switch value.Kind() {
	case reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return immutableIdentity{}
		}
		return immutableIdentity{kind: value.Kind(), ptr: value.Pointer()}
	default:
		return immutableIdentity{}
	}
}

// RegisterIncrementalImmutableInputs guards values returned after component entry.
func RegisterIncrementalImmutableInputs(ctx context.Context, values ...any) error {
	storage, err := incrementalImmutableStorage(ctx)
	if err != nil {
		return err
	}
	storage.add(values...)
	return nil
}

// RegisterIncrementalImmutableCertificate attaches a reusable guard to one render context.
func RegisterIncrementalImmutableCertificate(
	ctx context.Context,
	certificate *IncrementalImmutableCertificate,
) error {
	storage, err := incrementalImmutableStorage(ctx)
	if err != nil {
		return err
	}
	if certificate == nil {
		return errors.New("incremental immutable input certificate is unavailable")
	}
	storage.addCertificates(certificate)
	return nil
}

func incrementalImmutableStorage(ctx context.Context) (*immutableStorage, error) {
	if ctx == nil {
		return nil, errors.New("incremental immutable input binding is unavailable")
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage == nil {
		return nil, errors.New("incremental immutable input storage is unavailable")
	}
	return storage, nil
}

type immutableReferenceExtent struct {
	length   int
	capacity int
}

type immutableCertificateBuilder struct {
	visits immutableCertificateVisits
	ranges []immutableRange
}

type immutableCertificateVisits struct {
	smallIdentities [8]immutableIdentity
	smallExtents    [8]immutableReferenceExtent
	identities      []immutableIdentity
	extents         []immutableReferenceExtent
	count           int
	indexed         bool
}

func (b *immutableCertificateBuilder) certificate(values []any) *IncrementalImmutableCertificate {
	slices.SortFunc(b.ranges, func(left, right immutableRange) int {
		if byStart := cmp.Compare(left.start, right.start); byStart != 0 {
			return byStart
		}
		return cmp.Compare(left.end, right.end)
	})
	b.ranges = compactImmutableRanges(b.ranges)
	return newIncrementalImmutableCertificate(
		b.visits.certificateIdentities(), b.visits.indexed, b.ranges, slices.Clone(values),
	)
}

func newIncrementalImmutableCertificate(
	identitySlots []immutableIdentity,
	identityIndex bool,
	ranges []immutableRange,
	keep []any,
) *IncrementalImmutableCertificate {
	certificate := &IncrementalImmutableCertificate{}
	view := &incrementalImmutableCertificateView{
		certificate:   certificate,
		identitySlots: identitySlots,
		identityIndex: identityIndex,
		ranges:        ranges,
		keep:          keep,
	}
	certificate.seal = certificate
	certificate.view = view
	view.seal = view
	anchors := make([]immutableCertificateAnchor, len(keep))
	for index := range keep {
		anchors[index] = newImmutableCertificateAnchor(keep[index])
	}
	retained := slices.Clone(keep)
	view.proof = &incrementalImmutableCertificateViewProof{
		view: view, certificate: certificate, identitySlots: immutableCertificateSlice(identitySlots),
		identityIndex: identityIndex, ranges: immutableCertificateSlice(ranges),
		keep: immutableCertificateSlice(keep), registeredIdentitySlots: slices.Clone(identitySlots),
		registeredRanges: slices.Clone(ranges), retained: retained,
		retainedSlice: immutableCertificateSlice(retained), anchors: anchors,
	}
	view.proof.seal = view.proof
	certificate.proof = &incrementalImmutableCertificateProof{certificate: certificate, view: view}
	certificate.proof.seal = certificate.proof
	return certificate
}

func (v *immutableCertificateVisits) get(identity immutableIdentity) (immutableReferenceExtent, bool) {
	if !v.indexed {
		for index := range v.count {
			if v.smallIdentities[index] == identity {
				return v.smallExtents[index], true
			}
		}
		return immutableReferenceExtent{}, false
	}
	if len(v.identities) == 0 {
		return immutableReferenceExtent{}, false
	}
	index := immutableIdentityHash(identity) & uintptr(len(v.identities)-1)
	for {
		candidate := v.identities[index]
		if candidate.kind == reflect.Invalid {
			return immutableReferenceExtent{}, false
		}
		if candidate == identity {
			return v.extents[index], true
		}
		index = (index + 1) & uintptr(len(v.identities)-1)
	}
}

func (v *immutableCertificateVisits) set(identity immutableIdentity, extent immutableReferenceExtent) {
	if !v.indexed {
		for index := range v.count {
			if v.smallIdentities[index] == identity {
				v.smallExtents[index] = extent
				return
			}
		}
		if v.count < len(v.smallIdentities) {
			v.smallIdentities[v.count] = identity
			v.smallExtents[v.count] = extent
			v.count++
			return
		}
		v.grow()
	}
	if _, exists := v.get(identity); exists {
		v.insert(identity, extent)
		return
	}
	if (v.count+1)*10 >= len(v.identities)*7 {
		v.grow()
	}
	v.insert(identity, extent)
}

func (v *immutableCertificateVisits) grow() {
	capacity := 16
	if v.indexed {
		capacity = len(v.identities) * 2
	}
	previousIdentities := v.identities
	previousExtents := v.extents
	v.identities = make([]immutableIdentity, capacity)
	v.extents = make([]immutableReferenceExtent, capacity)
	previousCount := v.count
	v.count = 0
	if v.indexed {
		for index, identity := range previousIdentities {
			if identity.kind != reflect.Invalid {
				v.insert(identity, previousExtents[index])
			}
		}
	} else {
		v.indexed = true
		for index := range previousCount {
			v.insert(v.smallIdentities[index], v.smallExtents[index])
		}
	}
}

func (v *immutableCertificateVisits) certificateIdentities() []immutableIdentity {
	if v.indexed {
		return v.identities
	}
	return slices.Clone(v.smallIdentities[:v.count])
}

func (v *immutableCertificateVisits) insert(
	identity immutableIdentity,
	extent immutableReferenceExtent,
) {
	index := immutableIdentityHash(identity) & uintptr(len(v.identities)-1)
	for {
		candidate := &v.identities[index]
		if candidate.kind == reflect.Invalid {
			*candidate = identity
			v.extents[index] = extent
			v.count++
			return
		}
		if *candidate == identity {
			v.extents[index] = extent
			return
		}
		index = (index + 1) & uintptr(len(v.identities)-1)
	}
}

func immutableIdentityHash(identity immutableIdentity) uintptr {
	value := uint64(identity.ptr) ^ uint64(identity.kind)*0x9e3779b97f4a7c15
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	value ^= value >> 31
	return uintptr(value)
}

func compactImmutableRanges(ranges []immutableRange) []immutableRange {
	compacted := ranges[:0]
	for _, current := range ranges {
		if current.start >= current.end {
			continue
		}
		if len(compacted) == 0 || current.start > compacted[len(compacted)-1].end {
			compacted = append(compacted, current)
			continue
		}
		compacted[len(compacted)-1].end = max(compacted[len(compacted)-1].end, current.end)
	}
	return compacted
}

func (b *immutableCertificateBuilder) collectAny(value any) {
	if value == nil {
		return
	}
	if _, ok := value.(ResourceStore); ok {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		reference := reflect.ValueOf(typed)
		if !b.registerReference(reference) {
			return
		}
		for _, item := range typed {
			b.collectAny(item)
		}
	case []any:
		reference := reflect.ValueOf(typed)
		if !b.registerReference(reference) {
			return
		}
		start := reference.Pointer()
		b.ranges = append(b.ranges, immutableRange{
			start: start,
			end:   start + uintptr(reference.Cap())*reference.Type().Elem().Size(),
		})
		for _, item := range typed {
			b.collectAny(item)
		}
	default:
		b.collect(reflect.ValueOf(value))
	}
}

func (b *immutableCertificateBuilder) collect(value reflect.Value) {
	if value.IsValid() && immutableResourceStoreType(value.Type()) {
		return
	}
	value = immutableConcreteValue(value)
	if !value.IsValid() || immutableResourceStoreType(value.Type()) {
		return
	}
	if value.CanInterface() && (value.Type() == mapStringAnyType || value.Type() == sliceAnyType) {
		b.collectAny(value.Interface())
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if b.registerReference(value) {
			b.registerPointerRange(value)
			b.collect(value.Elem())
		}
	case reflect.Map:
		b.collectMap(value)
	case reflect.Slice:
		b.collectSlice(value)
	case reflect.Array:
		b.registerAddress(value)
		for index := range value.Len() {
			b.collect(value.Index(index))
		}
	case reflect.Struct:
		for index := range value.NumField() {
			b.collect(value.Field(index))
		}
	}
}

func (b *immutableCertificateBuilder) collectMap(value reflect.Value) {
	if !b.registerReference(value) {
		return
	}
	iterator := value.MapRange()
	for iterator.Next() {
		b.collect(iterator.Key())
		b.collect(iterator.Value())
	}
}

func (b *immutableCertificateBuilder) collectSlice(value reflect.Value) {
	if !b.registerReference(value) {
		return
	}
	start := value.Pointer()
	b.ranges = append(b.ranges, immutableRange{
		start: start,
		end:   start + uintptr(value.Cap())*value.Type().Elem().Size(),
	})
	for index := range value.Len() {
		b.collect(value.Index(index))
	}
}

func (b *immutableCertificateBuilder) registerPointerRange(value reflect.Value) {
	size := value.Type().Elem().Size()
	if size == 0 {
		return
	}
	start := value.Pointer()
	b.ranges = append(b.ranges, immutableRange{start: start, end: start + size})
}

func immutableResourceStoreType(typ reflect.Type) bool {
	return typ.NumMethod() != 0 && typ.Implements(resourceStoreType)
}

func (b *immutableCertificateBuilder) registerReference(value reflect.Value) bool {
	if value.IsNil() {
		return false
	}
	if value.Kind() == reflect.Slice && (value.Cap() == 0 || value.Type().Elem().Size() == 0) {
		return false
	}
	identity := immutableIdentity{kind: value.Kind(), ptr: value.Pointer()}
	extent := immutableReferenceExtent{}
	if value.Kind() == reflect.Slice {
		extent.length = value.Len()
		extent.capacity = value.Cap()
	}
	previous, exists := b.visits.get(identity)
	if exists && previous.length >= extent.length && previous.capacity >= extent.capacity {
		return false
	}
	b.visits.set(identity, immutableReferenceExtent{
		length:   max(previous.length, extent.length),
		capacity: max(previous.capacity, extent.capacity),
	})
	return true
}

func (b *immutableCertificateBuilder) registerAddress(value reflect.Value) {
	if !value.CanAddr() {
		return
	}
	identity := immutableIdentity{kind: reflect.Pointer, ptr: value.Addr().Pointer()}
	if _, exists := b.visits.get(identity); exists {
		return
	}
	b.visits.set(identity, immutableReferenceExtent{})
}

func (c *IncrementalImmutableCertificate) contains(target reflect.Value) bool {
	return c.containsTarget(newImmutableTarget(target))
}

func newImmutableTarget(target reflect.Value) immutableTarget {
	target = immutableConcreteValue(target)
	if !target.IsValid() {
		return immutableTarget{}
	}
	switch target.Kind() {
	case reflect.Map, reflect.Pointer, reflect.Slice:
		if target.IsNil() {
			return immutableTarget{}
		}
		pointer := target.Pointer()
		return immutableTarget{
			identity: immutableIdentity{kind: target.Kind(), ptr: pointer},
			pointer:  pointer,
		}
	default:
		if target.CanAddr() {
			pointer := target.Addr().Pointer()
			return immutableTarget{
				identity: immutableIdentity{kind: reflect.Pointer, ptr: pointer},
				pointer:  pointer,
			}
		}
	}
	return immutableTarget{}
}

func (c *IncrementalImmutableCertificate) containsTarget(target immutableTarget) bool {
	if !c.authenticate() || target.identity.kind == reflect.Invalid {
		return false
	}
	return c.view.containsIdentity(target.identity) || containsImmutableRange(c.view.ranges, target.pointer)
}

func (c *IncrementalImmutableCertificate) containsIdentity(identity immutableIdentity) bool {
	if !c.authenticate() {
		return false
	}
	return c.view.containsIdentity(identity)
}

func (v *incrementalImmutableCertificateView) containsRegisteredTarget(target immutableTarget) bool {
	if !v.authenticateRegistered() || target.identity.kind == reflect.Invalid {
		return false
	}
	return containsImmutableIdentity(
		v.proof.registeredIdentitySlots,
		v.proof.identityIndex,
		target.identity,
	) || containsImmutableRange(v.proof.registeredRanges, target.pointer)
}

func (v *incrementalImmutableCertificateView) containsIdentity(identity immutableIdentity) bool {
	return containsImmutableIdentity(v.identitySlots, v.identityIndex, identity)
}

func containsImmutableIdentity(
	identitySlots []immutableIdentity,
	identityIndex bool,
	identity immutableIdentity,
) bool {
	if len(identitySlots) == 0 {
		return false
	}
	if !identityIndex {
		return slices.Contains(identitySlots, identity)
	}
	index := immutableIdentityHash(identity) & uintptr(len(identitySlots)-1)
	for {
		candidate := identitySlots[index]
		if candidate.kind == reflect.Invalid {
			return false
		}
		if candidate == identity {
			return true
		}
		index = (index + 1) & uintptr(len(identitySlots)-1)
	}
}

func containsImmutableRange(ranges []immutableRange, pointer uintptr) bool {
	low, high := 0, len(ranges)
	for low < high {
		middle := int(uint(low+high) >> 1)
		candidate := ranges[middle]
		switch {
		case pointer < candidate.start:
			high = middle
		case pointer >= candidate.end:
			low = middle + 1
		default:
			return true
		}
	}
	return false
}

func (s *immutableStorage) addCertificates(certificates ...*IncrementalImmutableCertificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, certificate := range certificates {
		if certificate == nil || !certificate.authenticate() || s.hasCertificate(certificate) {
			continue
		}
		view := certificate.view
		if s.certified == nil && s.certifiedCount < len(s.certifiedSmall) {
			s.certifiedSmall[s.certifiedCount] = view
			s.certifiedCount++
			continue
		}
		if s.certified == nil {
			s.certified = newImmutableCertificateIndex(s.certifiedCount * 2)
			for _, small := range s.certifiedSmall[:s.certifiedCount] {
				s.certified.add(small)
			}
			s.certifiedSmall = [8]*incrementalImmutableCertificateView{}
			s.certifiedCount = 0
		}
		s.certified.add(view)
	}
}

func (s *immutableStorage) hasCertificate(certificate *IncrementalImmutableCertificate) bool {
	for index := range s.certifiedCount {
		if s.certifiedSmall[index].certificate == certificate {
			return true
		}
	}
	return s.certified.has(certificate)
}

func (s *immutableStorage) add(values ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := immutableVisitSet{}
	for _, value := range values {
		s.collect(reflect.ValueOf(value), &seen)
	}
	s.keep = append(s.keep, values...)
}

func (s *immutableStorage) addCapabilities(values ...any) {
	s.addCapabilityValues(true, values...)
}

func (s *immutableStorage) addBorrowedCapabilities(values ...any) {
	s.addCapabilityValues(false, values...)
}

func (s *immutableStorage) addCapabilityValues(retain bool, values ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := immutableVisitSet{}
	for _, value := range values {
		s.collectCapability(reflect.ValueOf(value), &seen)
	}
	if retain {
		s.keep = append(s.keep, values...)
	}
}

func (s *immutableStorage) collectCapability(value reflect.Value, seen *immutableVisitSet) {
	value = immutableConcreteValue(value)
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !s.registerReference(value, seen) {
			return
		}
		s.collectCapabilityPointee(value.Elem(), seen)
	case reflect.Map, reflect.Slice:
		s.registerReference(value, seen)
	case reflect.Struct:
		s.registerCapabilityValue(value, seen)
		for index := range value.NumField() {
			field := value.Field(index)
			if field.Kind() == reflect.Pointer {
				s.collectCapability(field, seen)
			}
		}
	}
}

func (s *immutableStorage) collectCapabilityPointee(elem reflect.Value, seen *immutableVisitSet) {
	s.registerCapabilityValue(elem, seen)
	if elem.Kind() != reflect.Struct {
		return
	}
	for index := range elem.NumField() {
		fieldType := elem.Type().Field(index)
		if fieldType.PkgPath != "" && !fieldType.Anonymous {
			continue
		}
		field := elem.Field(index)
		if field.Kind() == reflect.Pointer {
			s.collectCapability(field, seen)
		}
	}
}

func (s *immutableStorage) registerCapabilityValue(value reflect.Value, seen *immutableVisitSet) {
	if !value.CanAddr() {
		return
	}
	start := value.Addr().Pointer()
	visit := immutableVisit{identity: immutableIdentity{kind: reflect.Pointer, ptr: start}}
	if !seen.add(visit) {
		return
	}
	size := value.Type().Size()
	if size == 0 {
		s.addIdentity(visit.identity)
		return
	}
	s.ranges.insert(immutableRange{start: start, end: start + size})
}

func (s *immutableStorage) collect(value reflect.Value, seen *immutableVisitSet) {
	if s.containsInherited(value) {
		return
	}
	if value.IsValid() && value.Type().Implements(resourceStoreType) {
		return
	}
	value = immutableConcreteValue(value)
	if !value.IsValid() {
		return
	}
	if value.Type().Implements(resourceStoreType) {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		s.collectPointer(value, seen)
	case reflect.Map:
		s.collectMap(value, seen)
	case reflect.Slice:
		s.collectSlice(value, seen)
	case reflect.Array, reflect.Struct:
		s.registerAddress(value, seen)
		s.collectFields(value, seen)
	}
}

func (s *immutableStorage) containsInherited(target reflect.Value) bool {
	normalized := newImmutableTarget(target)
	for index := range s.certifiedCount {
		if s.certifiedSmall[index].containsRegisteredTarget(normalized) {
			return true
		}
	}
	if s.certified.contains(normalized) {
		return true
	}
	return s.parent != nil && s.parent.containsTarget(normalized)
}

func immutableConcreteValue(value reflect.Value) reflect.Value {
	for value.IsValid() && value.Kind() == reflect.Interface {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func (s *immutableStorage) registerReference(value reflect.Value, seen *immutableVisitSet) bool {
	if value.IsNil() {
		return false
	}
	if value.Kind() == reflect.Slice && (value.Cap() == 0 || value.Type().Elem().Size() == 0) {
		return false
	}
	identity := immutableIdentity{kind: value.Kind(), ptr: value.Pointer()}
	visit := immutableVisit{identity: identity}
	if value.Kind() == reflect.Slice {
		visit.length = value.Len()
		visit.capacity = value.Cap()
	}
	if !seen.add(visit) {
		return false
	}
	s.addIdentity(identity)
	return true
}

func (s *immutableStorage) registerAddress(value reflect.Value, seen *immutableVisitSet) {
	if !value.CanAddr() {
		return
	}
	identity := immutableIdentity{kind: reflect.Pointer, ptr: value.Addr().Pointer()}
	visit := immutableVisit{identity: identity}
	if !seen.add(visit) {
		return
	}
	s.addIdentity(identity)
}

func (s *immutableStorage) addIdentity(identity immutableIdentity) {
	if s.identities != nil {
		s.identities[identity] = struct{}{}
		return
	}
	for index := range s.identityCount {
		if s.identitySmall[index] == identity {
			return
		}
	}
	if s.identityCount < len(s.identitySmall) {
		s.identitySmall[s.identityCount] = identity
		s.identityCount++
		return
	}
	s.identities = make(map[immutableIdentity]struct{}, len(s.identitySmall)*2)
	for _, existing := range s.identitySmall {
		s.identities[existing] = struct{}{}
	}
	s.identities[identity] = struct{}{}
	s.identitySmall = [8]immutableIdentity{}
	s.identityCount = 0
}

func (s *immutableStorage) hasIdentity(identity immutableIdentity) bool {
	if s.identities != nil {
		_, exists := s.identities[identity]
		return exists
	}
	for index := range s.identityCount {
		if s.identitySmall[index] == identity {
			return true
		}
	}
	return false
}

func (s *immutableStorage) collectPointer(value reflect.Value, seen *immutableVisitSet) {
	if s.registerReference(value, seen) {
		s.collect(value.Elem(), seen)
	}
}

func (s *immutableStorage) collectMap(value reflect.Value, seen *immutableVisitSet) {
	if !s.registerReference(value, seen) {
		return
	}
	// collect descends only pointers, maps, slices, arrays, structs and the
	// interfaces that may hold them; every other kind falls through untouched.
	// A key type that can hold none of them — the string of every
	// map[string]any a resource-agnostic chart reads — therefore has nothing to
	// visit, while iter.Key() boxes a fresh reflect.Value per entry to be
	// discarded. That accounted for 70% of the map-iteration allocations in a
	// warm render.
	// map[string]any is the shape a resource-agnostic chart reads for every
	// object, so it is worth walking concretely: ranging the map itself boxes
	// nothing, and reflect.ValueOf on an element that is already an interface
	// unpacks it rather than allocating. iterator.Value() would box one per
	// entry.
	if typed, ok := immutableStringKeyedMap(value); ok {
		for _, element := range typed {
			s.collect(reflect.ValueOf(element), seen)
		}
		return
	}
	visitKeys := immutableKindHoldsReferences(value.Type().Key().Kind())
	iterator := value.MapRange()
	for iterator.Next() {
		if visitKeys {
			s.collect(iterator.Key(), seen)
		}
		s.collect(iterator.Value(), seen)
	}
}

// immutableStringKeyedMap returns the map as map[string]any when it is exactly
// that type. A named type with the same underlying shape is deliberately not
// accepted: Interface would have to convert it, which allocates the very box
// this avoids.
func immutableStringKeyedMap(value reflect.Value) (map[string]any, bool) {
	if value.Type() != stringKeyedAnyMapType || !value.CanInterface() {
		return nil, false
	}
	typed, ok := value.Interface().(map[string]any)
	return typed, ok
}

var stringKeyedAnyMapType = reflect.TypeOf(map[string]any(nil))

// immutableKindHoldsReferences reports whether collect can descend into a kind.
// It must stay in step with collect's switch, plus Interface, which unwraps to
// any of them.
func immutableKindHoldsReferences(kind reflect.Kind) bool {
	switch kind {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Interface:
		return true
	default:
		return false
	}
}

func (s *immutableStorage) collectSlice(value reflect.Value, seen *immutableVisitSet) {
	if !s.registerReference(value, seen) {
		return
	}
	if value.Pointer() != 0 && value.Cap() > 0 && value.Type().Elem().Size() > 0 {
		start := value.Pointer()
		s.ranges.insert(immutableRange{
			start: start,
			end:   start + uintptr(value.Cap())*value.Type().Elem().Size(),
		})
	}
	s.collectFields(value, seen)
}

func (s *immutableStorage) collectFields(value reflect.Value, seen *immutableVisitSet) {
	if value.Kind() == reflect.Struct {
		for index := range value.NumField() {
			s.collect(value.Field(index), seen)
		}
		return
	}
	for index := range value.Len() {
		s.collect(value.Index(index), seen)
	}
}

func (s *immutableStorage) contains(target reflect.Value) bool {
	return s.containsTarget(newImmutableTarget(target))
}

func (s *immutableStorage) containsTarget(target immutableTarget) bool {
	s.mu.RLock()
	if s.containsLocalTarget(target) {
		s.mu.RUnlock()
		return true
	}
	for index := range s.certifiedCount {
		if s.certifiedSmall[index].containsRegisteredTarget(target) {
			s.mu.RUnlock()
			return true
		}
	}
	if s.certified.contains(target) {
		s.mu.RUnlock()
		return true
	}
	s.mu.RUnlock()
	if s.parent != nil {
		return s.parent.containsTarget(target)
	}
	return false
}

func (s *immutableStorage) containsLocalTarget(target immutableTarget) bool {
	if target.identity.kind == reflect.Invalid {
		return false
	}
	if s.hasIdentity(target.identity) {
		return true
	}
	if s.ranges.contains(target.pointer) {
		return true
	}
	return false
}

func (i *immutableRangeIndex) insert(value immutableRange) {
	if value.start >= value.end {
		return
	}
	if i.root == nil && i.count < len(i.small) {
		i.small[i.count] = value
		i.count++
		return
	}
	if i.root == nil {
		for index := range i.count {
			i.root = insertImmutableRange(i.root, i.small[index])
		}
		i.small = [4]immutableRange{}
		i.count = 0
	}
	i.root = insertImmutableRange(i.root, value)
}

func (i *immutableRangeIndex) contains(pointer uintptr) bool {
	for index := range i.count {
		value := i.small[index]
		if pointer >= value.start && pointer < value.end {
			return true
		}
	}
	for node := i.root; node != nil; {
		if pointer >= node.rangeValue.start && pointer < node.rangeValue.end {
			return true
		}
		if node.left != nil && node.left.maxEnd > pointer {
			node = node.left
			continue
		}
		node = node.right
	}
	return false
}

func insertImmutableRange(node *immutableRangeNode, value immutableRange) *immutableRangeNode {
	if node == nil {
		return &immutableRangeNode{rangeValue: value, maxEnd: value.end, height: 1}
	}
	switch {
	case value.start < node.rangeValue.start:
		node.left = insertImmutableRange(node.left, value)
	case value.start > node.rangeValue.start:
		node.right = insertImmutableRange(node.right, value)
	case value.end > node.rangeValue.end:
		node.rangeValue.end = value.end
	default:
		return node
	}
	return balanceImmutableRangeNode(node)
}

func balanceImmutableRangeNode(node *immutableRangeNode) *immutableRangeNode {
	refreshImmutableRangeNode(node)
	balance := immutableRangeHeight(node.left) - immutableRangeHeight(node.right)
	if balance > 1 {
		if immutableRangeHeight(node.left.left) < immutableRangeHeight(node.left.right) {
			node.left = rotateImmutableRangeLeft(node.left)
		}
		return rotateImmutableRangeRight(node)
	}
	if balance < -1 {
		if immutableRangeHeight(node.right.right) < immutableRangeHeight(node.right.left) {
			node.right = rotateImmutableRangeRight(node.right)
		}
		return rotateImmutableRangeLeft(node)
	}
	return node
}

func rotateImmutableRangeLeft(node *immutableRangeNode) *immutableRangeNode {
	root := node.right
	node.right = root.left
	root.left = node
	refreshImmutableRangeNode(node)
	refreshImmutableRangeNode(root)
	return root
}

func rotateImmutableRangeRight(node *immutableRangeNode) *immutableRangeNode {
	root := node.left
	node.left = root.right
	root.right = node
	refreshImmutableRangeNode(node)
	refreshImmutableRangeNode(root)
	return root
}

func refreshImmutableRangeNode(node *immutableRangeNode) {
	node.height = max(immutableRangeHeight(node.left), immutableRangeHeight(node.right)) + 1
	node.maxEnd = max(node.rangeValue.end, immutableRangeMaxEnd(node.left), immutableRangeMaxEnd(node.right))
}

func immutableRangeHeight(node *immutableRangeNode) int {
	if node == nil {
		return 0
	}
	return node.height
}

func immutableRangeMaxEnd(node *immutableRangeNode) uintptr {
	if node == nil {
		return 0
	}
	return node.maxEnd
}

func (s *immutableStorage) containsReachable(value reflect.Value, seen *immutableVisitSet) bool {
	value = immutableConcreteValue(value)
	if !value.IsValid() {
		return false
	}
	if s.contains(value) {
		return true
	}
	switch value.Kind() {
	case reflect.Pointer:
		return s.containsReachablePointer(value, seen)
	case reflect.Map:
		return s.containsReachableMap(value, seen)
	case reflect.Slice:
		return s.containsReachableSlice(value, seen)
	case reflect.Array:
		return s.containsReachableArray(value, seen)
	case reflect.Struct:
		return s.containsReachableStruct(value, seen)
	}
	return false
}

func (s *immutableStorage) containsReachablePointer(value reflect.Value, seen *immutableVisitSet) bool {
	return markImmutableVisit(value, seen) && s.containsReachable(value.Elem(), seen)
}

func (s *immutableStorage) containsReachableMap(value reflect.Value, seen *immutableVisitSet) bool {
	if !markImmutableVisit(value, seen) {
		return false
	}
	iterator := value.MapRange()
	for iterator.Next() {
		if s.containsReachable(iterator.Key(), seen) || s.containsReachable(iterator.Value(), seen) {
			return true
		}
	}
	return false
}

func (s *immutableStorage) containsReachableSlice(value reflect.Value, seen *immutableVisitSet) bool {
	if !markImmutableVisit(value, seen) {
		return false
	}
	return s.containsReachableArray(value, seen)
}

func (s *immutableStorage) containsReachableArray(value reflect.Value, seen *immutableVisitSet) bool {
	for index := range value.Len() {
		if s.containsReachable(value.Index(index), seen) {
			return true
		}
	}
	return false
}

func (s *immutableStorage) containsReachableStruct(value reflect.Value, seen *immutableVisitSet) bool {
	for index := range value.NumField() {
		if s.containsReachable(value.Field(index), seen) {
			return true
		}
	}
	return false
}

func markImmutableVisit(value reflect.Value, seen *immutableVisitSet) bool {
	if value.IsNil() {
		return false
	}
	visit := immutableVisit{
		identity: immutableIdentity{kind: value.Kind(), ptr: value.Pointer()},
	}
	if value.Kind() == reflect.Slice {
		visit.length = value.Len()
		visit.capacity = value.Cap()
	}
	return seen.add(visit)
}

func immutableMutationError(path string, line int) error {
	if path == "" {
		return fmt.Errorf("template mutates an immutable input")
	}
	return fmt.Errorf("template mutates an immutable input at %s:%d", path, line)
}

func immutableNativeMutationError(env native.Env, target any) error {
	ctx := env.Context()
	if ctx == nil {
		return nil
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage == nil || !storage.contains(reflect.ValueOf(target)) {
		return nil
	}
	return immutableMutationError(env.CallPath(), env.CallLine())
}

func immutableNativeInputError(env native.Env, values ...any) error {
	ctx := env.Context()
	if ctx == nil {
		return nil
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage == nil {
		return nil
	}
	for _, value := range values {
		seen := immutableVisitSet{}
		if storage.containsReachable(reflect.ValueOf(value), &seen) {
			return immutableMutationError(env.CallPath(), env.CallLine())
		}
	}
	return nil
}

func observeIncrementalMutation(ctx context.Context, mutation scriggo.Mutation) error {
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	if storage == nil || !storage.contains(mutation.Target) {
		return nil
	}
	return immutableMutationError(mutation.Path, mutation.Line)
}

func observeIncrementalNativeCall(ctx context.Context, call scriggo.NativeCall) error {
	if ctx == nil || nativeMethodPreservesImmutableInputs(call.Receiver, call.Method) {
		return nil
	}
	storage, _ := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	seen := immutableVisitSet{}
	if storage == nil || !storage.containsReachable(call.Receiver, &seen) {
		return nil
	}
	return immutableMutationError(call.Path, call.Line)
}

func nativeMethodPreservesImmutableInputs(receiver reflect.Value, method string) bool {
	for receiver.IsValid() {
		if allowed, matched := nativeReceiverMethodPreservesImmutableInputs(receiver.Type(), method); matched {
			return allowed
		}
		if receiver.Kind() != reflect.Interface || receiver.IsNil() {
			return false
		}
		receiver = receiver.Elem()
	}
	return false
}

func nativeReceiverMethodPreservesImmutableInputs(
	typ reflect.Type,
	method string,
) (allowed, matched bool) {
	if typ == builtinRegexpType {
		return preservingRegexpMethod(method), true
	}
	if typ == reflect.TypeFor[*sharedContributionContext]() {
		return preservingSharedContributionMethod(method), true
	}
	if typ == reflect.TypeFor[*PathResolver]() {
		return method == "GetBaseDir" || method == "GetPath", true
	}
	if typ == reflect.TypeFor[*SharedContext]() && (method == "Get" || method == "ComputeIfAbsent") {
		return true, true
	}
	if typ.Implements(resourceStoreType) || typ.Implements(httpFetcherType) {
		return true, true
	}
	if typ.Implements(fileRegistrarType) {
		return method == "Register", true
	}
	if typ.Implements(planRegistrarType) {
		return preservingPlanRegistrarMethod(method), true
	}
	if typ.Implements(incrementalBackendPlanRegistrarType) {
		return method == memberProfile || method == memberBackend || method == "BackendWhenAny", true
	}
	return false, false
}

func preservingRegexpMethod(method string) bool {
	switch method {
	case "Match", "Find", "FindAll", "FindAllSubmatch", "FindSubmatch",
		memberReplaceAll, "ReplaceAllFunc", "Split":
		return true
	default:
		return false
	}
}

func preservingSharedContributionMethod(method string) bool {
	switch method {
	case "Unique", "Publish", "PublishRanked", "Select", "SelectValues", "Count":
		return true
	default:
		return false
	}
}

func preservingPlanRegistrarMethod(method string) bool {
	switch method {
	case "Section", "Fragment", memberProfile, memberBackend, "ProfileGroup", "MapMeta", "SensitiveMap":
		return true
	default:
		return false
	}
}
