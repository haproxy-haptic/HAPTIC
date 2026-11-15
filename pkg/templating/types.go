// Package templating provides template rendering capabilities using the Scriggo
// template engine.
//
// This package offers a unified interface for compiling and rendering templates
// using Go template syntax via the Scriggo engine.
//
// Templates are pre-compiled at initialization for optimal runtime performance
// and early detection of syntax errors.
package templating

// EngineNameScriggo is the canonical name of the (only) supported template
// engine. Used to validate the configured engine string in callers.
const EngineNameScriggo = "scriggo"

// FileRegistrar is an interface for dynamic file registration during template rendering.
// This interface is implemented by rendercontext.FileRegistry, allowing templates to
// register auxiliary files (certificates, maps, etc.) without creating import cycles.
//
// The Register method signature matches the variadic calling convention used in templates:
//
//	file_registry.Register("cert", "filename.pem", "content...")
//
// Arguments:
//   - args[0]: file type (string) - "cert", "map", "file", or "crt-list"
//   - args[1]: filename (string) - base filename
//   - args[2]: content (string) - file content
//
// Returns:
//   - Predicted absolute path where the file will be located
//   - Error if validation fails or content conflict detected
type FileRegistrar interface {
	Register(args ...any) (string, error)
}

// PlanRegistrar lets templates declare the structure of the configuration they
// emit: which text is one section, which backend record a section was built
// from, where the shared profiles go, and how a map file is ordered. It is
// implemented by rendercontext.PlanRegistry (which also assembles the final
// config from the returned tokens), keeping the plan types out of this package.
//
// Templates use it through the `planRegistry` global:
//
//	{%- var name, err = planRegistry.Profile(record) -%}
//	{%- var token, err = planRegistry.Section("profile", name, body) -%}
//	{%- var token, err = planRegistry.Backend(record, text) -%}
//	{{ planRegistry.ProfileGroup() }}
//	{%- var err = planRegistry.MapMeta("host.map", false) -%}
//
// Section, Backend and ProfileGroup return a placeholder line that the
// assembler replaces with the registered text; nothing else may emit one.
type PlanRegistrar interface {
	// Section registers section text under a kind ("profile" or "backend")
	// and a name, and returns its placeholder line. Registering the same
	// (kind, name) twice is fine while the text is identical.
	Section(kind, name, text string) (string, error)

	// Fragment registers text to splice in at the placeholder it returns,
	// without the text passing through the template writer. Unlike Section it
	// does not partition the config — the fragment lands inside the
	// surrounding core section, so the rendered bytes are identical to
	// emitting the text inline.
	Fragment(name string, fragment TextFragment) (string, error)

	// Profile content-addresses a named `defaults` section from the shape
	// values a backend shares with every backend of the same shape (mode,
	// balance, hash-type, default-server keywords and the profile directive
	// lines), registers it, and returns its name (`haptic-be-<hash>`). Two
	// backends with the same shape get the same name and one section, which is
	// what makes them dynamic-eligible. The name is what Backend() writes after
	// `from` and records as the backend's `profile`.
	Profile(record map[string]any) (string, error)

	// Backend records a backend as data and registers its section text.
	// Unknown keys and a missing name are errors — a typo must not silently
	// under-describe the emitted section.
	Backend(record map[string]any, text string) (string, error)

	// ProfileGroup returns the placeholder line where every registered
	// profile section is spliced, sorted by name. Emit it exactly once.
	ProfileGroup() string

	// MapMeta declares whether entry order matters for a map file. Maps are
	// ordered unless declared otherwise.
	MapMeta(path string, ordered bool) error

	// SensitiveMap declares that a map file holds credentials for the whole
	// render: published as a Secret and replaced only as a whole at runtime.
	SensitiveMap(path string) error
}

// IncrementalBackendPlanRegistrar records deterministic backend declarations.
type IncrementalBackendPlanRegistrar interface {
	Profile(record map[string]any) (string, error)
	Backend(record map[string]any, text string) (string, error)
	BackendWhenAny(record map[string]any, text, cell string, keys []string) (string, error)
}

// ResourceStore defines the interface for resource stores accessible from templates.
// This interface enables direct method calls in Scriggo templates:
//
//	{% for _, ing := range resources.ingresses.List() %}
//	{% var secret = resources.secrets.GetSingle(namespace, name) %}
//	{% for _, ep := range resources.endpoints.Fetch(serviceName) %}
//
// Scriggo supports dot notation for map access, so `resources.ingresses` is equivalent
// to `resources["ingresses"]`.
//
// Implementations are provided by pkg/controller/rendercontext.StoreWrapper.
type ResourceStore interface {
	// List returns all resources from the store.
	List() []any

	// Fetch returns resources matching the given keys (typically namespace, name).
	Fetch(keys ...any) []any

	// GetSingle returns a single resource matching the keys, or nil if not found.
	GetSingle(keys ...any) any
}

// HTTPFetcher defines the interface for HTTP resource fetching accessible from templates.
// This interface enables the http.Fetch() method in Scriggo templates:
//
//	{% var content = http.Fetch("https://example.com/blocklist.txt") %}
//	{% var content = http.Fetch(url, map[string]any{"interval": "60s", "critical": true}) %}
//
// Implementations are provided by pkg/controller/httpstore.HTTPStoreWrapper.
type HTTPFetcher interface {
	// Fetch fetches content from a URL with optional options and authentication.
	// Arguments:
	//   - args[0]: URL (string, required)
	//   - args[1]: options (map, optional) - {"interval": "60s", "timeout": "30s", "retries": 3, "critical": true}
	//   - args[2]: auth (map, optional) - {"type": "bearer"|"basic", "token": "...", ...}
	Fetch(args ...any) (any, error)

	// Pending reports whether this render left the URL out because its new
	// content waits for a render that can accept it: Fetch returned empty
	// content without fetching, and a later render serves the content.
	Pending(url string) bool
}

// RuntimeEnvironment holds runtime information available to templates.
// This enables templates to adapt behavior based on the execution environment.
//
// Templates access this via the runtimeEnvironment variable:
//
//	{%- var maxShards = runtimeEnvironment.GOMAXPROCS * 2 %}
//
// Fields use exported names for direct template access.
type RuntimeEnvironment struct {
	// GOMAXPROCS is the maximum number of OS threads for parallel execution.
	// Used by sharding logic to calculate optimal shard count.
	GOMAXPROCS int
}
