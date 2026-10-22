package rendercontext

import (
	"fmt"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

// NamespaceScope limits a watched resource's keyed lookups to the namespaces
// its watch reads. A nil scope allows every namespace.
type NamespaceScope struct {
	resource   string
	namespaces []string
	setting    string
}

// namespaceScopeOf returns the scope of a watched resource, or nil when its
// watch reads every namespace.
func namespaceScopeOf(name string, resource *config.WatchedResource) *NamespaceScope {
	// Config validation requires namespace-first indexing for a scoped watch.
	if len(resource.Namespaces) == 0 || len(resource.IndexBy) == 0 || resource.IndexBy[0] != config.NamespaceIndexPath {
		return nil
	}
	setting := resource.NamespacesSetting
	if setting == "" {
		setting = "watchedResources." + name + ".namespaces"
	}
	return &NamespaceScope{resource: name, namespaces: resource.Namespaces, setting: setting}
}

// Check rejects lookup keys whose namespace the watch never reads: the store
// holds nothing there, so an empty result would read as "not found".
func (s *NamespaceScope) Check(keys []string) error {
	if s == nil || len(keys) == 0 || slices.Contains(s.namespaces, keys[0]) {
		return nil
	}
	target := "in namespace " + keys[0]
	if len(keys) > 1 {
		target = keys[0] + "/" + keys[1]
	}
	allowed := strings.Join(s.namespaces, ", ")
	return &NamespaceScopeError{message: fmt.Sprintf("%s %s cannot be read: %s limits the %s watch to %s. Reference %s in %s, or change %s",
		s.resource, target, s.setting, s.resource, allowed, s.resource, allowed, s.setting)}
}

// NamespaceScopes returns the scope of each watched resource of cfg, or nil
// when every watch reads all namespaces.
func NamespaceScopes(cfg *config.Config) func(string) *NamespaceScope {
	scopes := make(map[string]*NamespaceScope)
	for name := range cfg.WatchedResources {
		resource := cfg.WatchedResources[name]
		if scope := namespaceScopeOf(name, &resource); scope != nil {
			scopes[name] = scope
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	return func(name string) *NamespaceScope { return scopes[name] }
}

// NamespaceScopeError identifies a lookup forbidden by configured watch namespaces.
type NamespaceScopeError struct {
	message string
}

func (e *NamespaceScopeError) Error() string { return e.message }

// IsNamespaceScopeViolation excludes mixed failures so negative tests cannot hide bad inputs.
func IsNamespaceScopeViolation(err error) bool {
	switch cause := err.(type) {
	case *NamespaceScopeError:
		return true
	case interface{ Unwrap() []error }:
		causes := cause.Unwrap()
		return len(causes) > 0 && !slices.ContainsFunc(causes, func(child error) bool {
			return !IsNamespaceScopeViolation(child)
		})
	case interface{ Unwrap() error }:
		return IsNamespaceScopeViolation(cause.Unwrap())
	default:
		return false
	}
}
