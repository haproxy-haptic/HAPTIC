package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a Config that passes ValidateStructure; each test mutates
// only the field it is about.
func validConfig() *Config {
	return &Config{
		PodSelector:      PodSelector{MatchLabels: map[string]string{"app": "haproxy"}},
		Logging:          LoggingConfig{Level: "INFO"},
		Dataplane:        DataplaneConfig{Port: 5555, MapsDir: "/etc/haproxy/maps", SSLCertsDir: "/etc/haproxy/certs", GeneralStorageDir: "/etc/haproxy/general", SecretsDir: "/etc/haproxy/secrets", ConfigFile: "/etc/haproxy/haproxy.cfg"},
		WatchedResources: map[string]WatchedResource{"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace"}}},
		HAProxyConfig:    HAProxyConfig{Template: "global"},
	}
}

func TestValidateStructure_Success(t *testing.T) {
	cfg := validConfig()

	err := ValidateStructure(cfg)
	assert.NoError(t, err)
}

func TestValidateStructure_NilConfig(t *testing.T) {
	err := ValidateStructure(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config is nil")
}

func TestValidateStructure_RejectsInvalidLeaderElectionDurations(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*LeaderElectionConfig)
		want      string
	}{
		{
			name: "invalid lease duration",
			configure: func(cfg *LeaderElectionConfig) {
				cfg.LeaseDuration = "later"
			},
			want: "lease_duration",
		},
		{
			name: "non-positive renew deadline",
			configure: func(cfg *LeaderElectionConfig) {
				cfg.RenewDeadline = "0s"
			},
			want: "renew_deadline must be greater than zero",
		},
		{
			name: "renew deadline reaches lease duration",
			configure: func(cfg *LeaderElectionConfig) {
				cfg.LeaseDuration = "10s"
				cfg.RenewDeadline = "10s"
			},
			want: "lease_duration must be greater than renew_deadline",
		},
		{
			name: "renew deadline inside retry jitter",
			configure: func(cfg *LeaderElectionConfig) {
				cfg.LeaseDuration = "30s"
				cfg.RenewDeadline = "6s"
				cfg.RetryPeriod = "5s"
			},
			want: "renew_deadline must be more than 20% greater than retry_period",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.configure(&cfg.Controller.LeaderElection)
			assert.ErrorContains(t, ValidateStructure(cfg), test.want)
		})
	}
}

func TestValidateStructure_AcceptsLeaderElectionDurations(t *testing.T) {
	cfg := validConfig()
	cfg.Controller.LeaderElection = LeaderElectionConfig{
		LeaseDuration: "30s",
		RenewDeadline: "20s",
		RetryPeriod:   "5s",
	}
	assert.NoError(t, ValidateStructure(cfg))
}

func TestValidateStructure_RejectsInvalidValidatorGlobs(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*ValidatorConfig)
		want      string
	}{
		{
			name: "files",
			configure: func(validator *ValidatorConfig) {
				validator.Files = []string{"general/[broken"}
			},
			want: "validators: spoa-hub.files: invalid glob",
		},
		{
			name: "data files",
			configure: func(validator *ValidatorConfig) {
				validator.DataFiles = []string{"general/[broken"}
			},
			want: "validators: spoa-hub.data_files: invalid glob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			validator := ValidatorConfig{
				Name:       "spoa-hub",
				SocketPath: "/var/run/haptic-validators/spoa-hub.sock",
				Files:      []string{"general/*.toml"},
			}
			tt.configure(&validator)
			cfg.Validators = []ValidatorConfig{validator}

			err := ValidateStructure(cfg)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestValidatePodSelector_EmptyMatchLabels(t *testing.T) {
	cfg := validConfig()
	cfg.PodSelector.MatchLabels = map[string]string{}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "match_labels cannot be empty")
}

func TestValidatePodSelector_EmptyLabelKey(t *testing.T) {
	cfg := validConfig()
	cfg.PodSelector.MatchLabels = map[string]string{"": "value"}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "match_labels key cannot be empty")
}

func TestValidatePodSelector_EmptyLabelValue(t *testing.T) {
	cfg := validConfig()
	cfg.PodSelector.MatchLabels = map[string]string{"app": ""}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "match_labels value")
}

func TestValidateLoggingConfig_InvalidLevel(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"invalid string", "INVALID"},
		{"numeric string", "1"},
		{"typo", "DEBG"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Logging.Level = tt.level

			err := ValidateStructure(cfg)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "level")
		})
	}
}

func TestValidateLoggingConfig_ValidLevels(t *testing.T) {
	validLevels := []string{"", "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "trace", "debug", "info", "warn", "error"}

	for _, level := range validLevels {
		t.Run("level_"+level, func(t *testing.T) {
			cfg := validConfig()
			cfg.Logging.Level = level

			err := ValidateStructure(cfg)
			assert.NoError(t, err)
		})
	}
}

func TestValidateWatchedResources_Empty(t *testing.T) {
	cfg := validConfig()
	cfg.WatchedResources = map[string]WatchedResource{}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one resource must be configured")
}

func TestValidateWatchedResource_MissingAPIVersion(t *testing.T) {
	cfg := validConfig()
	cfg.WatchedResources["ingresses"] = WatchedResource{Resources: "ingresses", IndexBy: []string{"metadata.namespace"}}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "api_version")
}

func TestValidateWatchedResource_MissingKind(t *testing.T) {
	cfg := validConfig()
	cfg.WatchedResources["ingresses"] = WatchedResource{APIVersion: "networking.k8s.io/v1", Resources: "", IndexBy: []string{"metadata.namespace"}}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resources")
}

func TestValidateWatchedResource_EmptyIndexBy(t *testing.T) {
	cfg := validConfig()
	cfg.WatchedResources["ingresses"] = WatchedResource{APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{}}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "index_by")
}

func TestValidateWatchedResource_EmptyIndexByElement(t *testing.T) {
	cfg := validConfig()
	cfg.WatchedResources["ingresses"] = WatchedResource{APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace", ""}}

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "index_by[1] cannot be empty")
}

func TestValidateHAProxyConfig_EmptyTemplate(t *testing.T) {
	cfg := validConfig()
	cfg.HAProxyConfig.Template = ""

	err := ValidateStructure(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "template cannot be empty")
}

func TestValidateCredentials_Success(t *testing.T) {
	creds := &Credentials{
		DataplaneUsername: "admin",
		DataplanePassword: "pass",
	}

	err := ValidateCredentials(creds)
	assert.NoError(t, err)
}

func TestValidateCredentials_Nil(t *testing.T) {
	err := ValidateCredentials(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "credentials are nil")
}

func TestValidateCredentials_MissingFields(t *testing.T) {
	tests := []struct {
		name     string
		creds    *Credentials
		errField string
	}{
		{
			name: "missing dataplane_username",
			creds: &Credentials{
				DataplaneUsername: "",
				DataplanePassword: "pass",
			},
			errField: "dataplane_username",
		},
		{
			name: "missing dataplane_password",
			creds: &Credentials{
				DataplaneUsername: "admin",
				DataplanePassword: "",
			},
			errField: "dataplane_password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCredentials(tt.creds)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.errField)
		})
	}
}

func TestValidateDataplaneConfig_InvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"zero", 0},
		{"negative", -1},
		{"too large", 65536},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Dataplane.Port = tt.port

			err := ValidateStructure(cfg)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "port must be between 1 and 65535")
		})
	}
}

func TestValidateDataplaneConfig_EmptyPaths(t *testing.T) {
	tests := []struct {
		name      string
		mapsDir   string
		sslDir    string
		genDir    string
		cfgFile   string
		errSubstr string
	}{
		{
			name:      "empty maps_dir",
			mapsDir:   "",
			sslDir:    "/etc/haproxy/certs",
			genDir:    "/etc/haproxy/general",
			cfgFile:   "/etc/haproxy/haproxy.cfg",
			errSubstr: "maps_dir cannot be empty",
		},
		{
			name:      "empty ssl_certs_dir",
			mapsDir:   "/etc/haproxy/maps",
			sslDir:    "",
			genDir:    "/etc/haproxy/general",
			cfgFile:   "/etc/haproxy/haproxy.cfg",
			errSubstr: "ssl_certs_dir cannot be empty",
		},
		{
			name:      "empty general_storage_dir",
			mapsDir:   "/etc/haproxy/maps",
			sslDir:    "/etc/haproxy/certs",
			genDir:    "",
			cfgFile:   "/etc/haproxy/haproxy.cfg",
			errSubstr: "general_storage_dir cannot be empty",
		},
		{
			name:      "empty config_file",
			mapsDir:   "/etc/haproxy/maps",
			sslDir:    "/etc/haproxy/certs",
			genDir:    "/etc/haproxy/general",
			cfgFile:   "",
			errSubstr: "config_file cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Dataplane.MapsDir = tt.mapsDir
			cfg.Dataplane.SSLCertsDir = tt.sslDir
			cfg.Dataplane.GeneralStorageDir = tt.genDir
			cfg.Dataplane.ConfigFile = tt.cfgFile

			err := ValidateStructure(cfg)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

// versionFieldsBaseConfig returns a minimal valid config whose single watched
// resource is overridden per test case.
func versionFieldsBaseConfig(res *WatchedResource) *Config {
	return &Config{
		PodSelector: PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		Dataplane: DataplaneConfig{
			Port:              5555,
			MapsDir:           "/etc/haproxy/maps",
			SSLCertsDir:       "/etc/haproxy/certs",
			GeneralStorageDir: "/etc/haproxy/general",
			SecretsDir:        "/etc/haproxy/secrets",
			ConfigFile:        "/etc/haproxy/haproxy.cfg",
		},
		WatchedResources: map[string]WatchedResource{"routes": *res},
		HAProxyConfig:    HAProxyConfig{Template: "global"},
	}
}

func TestValidateWatchedResource_VersionFields(t *testing.T) {
	tests := []struct {
		name      string
		resource  WatchedResource
		errSubstr string // empty = expect success
	}{
		{
			name: "api_versions list alone is valid",
			resource: WatchedResource{
				APIVersions: []string{"example.io/v1", "example.io/v1beta1"},
				Resources:   "routes",
				IndexBy:     []string{"metadata.name"},
			},
		},
		{
			name: "api_version and api_versions are mutually exclusive",
			resource: WatchedResource{
				APIVersion:  "example.io/v1",
				APIVersions: []string{"example.io/v1beta1"},
				Resources:   "routes",
				IndexBy:     []string{"metadata.name"},
			},
			errSubstr: "mutually exclusive",
		},
		{
			name: "empty api_versions element is rejected",
			resource: WatchedResource{
				APIVersions: []string{"example.io/v1", ""},
				Resources:   "routes",
				IndexBy:     []string{"metadata.name"},
			},
			errSubstr: "api_versions[1] cannot be empty",
		},
		{
			name: "neither field set is rejected",
			resource: WatchedResource{
				Resources: "routes",
				IndexBy:   []string{"metadata.name"},
			},
			errSubstr: "one of api_version or api_versions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStructure(versionFieldsBaseConfig(&tt.resource))
			if tt.errSubstr == "" {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestValidateWatchedResource_Namespaces(t *testing.T) {
	scoped := func(indexBy []string, namespaces ...string) WatchedResource {
		return WatchedResource{APIVersion: "v1", Resources: "secrets", IndexBy: indexBy, Namespaces: namespaces}
	}
	byNamespace := []string{"metadata.namespace", "metadata.name"}
	tests := []struct {
		name      string
		resource  WatchedResource
		errSubstr string // empty = expect success
	}{
		{name: "unset watches all namespaces", resource: WatchedResource{APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.name"}}},
		{name: "listed namespaces", resource: scoped(byNamespace, "team-a", "team-b")},
		{name: "empty list is rejected", resource: WatchedResource{APIVersion: "v1", Resources: "secrets", IndexBy: byNamespace, Namespaces: []string{}}, errSubstr: "namespaces is empty"},
		{name: "index must start with the namespace", resource: scoped([]string{"metadata.name", "metadata.namespace"}, "team-a"), errSubstr: "namespaces requires index_by to start with metadata.namespace"},
		{name: "invalid name", resource: scoped(byNamespace, "Team_A"), errSubstr: `namespaces[0] "Team_A" is not a valid namespace name`},
		{name: "empty name", resource: scoped(byNamespace, "team-a", ""), errSubstr: `namespaces[1] "" is not a valid namespace name`},
		{name: "duplicate", resource: scoped(byNamespace, "team-a", "team-a"), errSubstr: `namespaces lists "team-a" twice`},
		{name: "setting describes listed namespaces", resource: WatchedResource{APIVersion: "v1", Resources: "secrets", IndexBy: byNamespace, Namespaces: []string{"team-a"}, NamespacesSetting: "chart.value"}},
		{name: "setting without namespaces", resource: WatchedResource{APIVersion: "v1", Resources: "secrets", IndexBy: byNamespace, NamespacesSetting: "chart.value"}, errSubstr: "namespaces_setting has no namespaces to describe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStructure(versionFieldsBaseConfig(&tt.resource))
			if tt.errSubstr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestValidateRequires(t *testing.T) {
	base := func() *Config {
		return versionFieldsBaseConfig(&WatchedResource{
			APIVersion: "example.io/v1",
			Resources:  "routes",
			IndexBy:    []string{"metadata.name"},
		})
	}

	t.Run("requires naming a watched resource is valid", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {Name: "snippet-a", Template: "x", Requires: []string{"routes"}},
		}
		cfg.ValidationTests = map[string]ValidationTest{
			"test-a": {Requires: []string{"routes"}, Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
		}
		assert.NoError(t, ValidateStructure(cfg))
	})

	t.Run("dangling snippet requires is rejected", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {Name: "snippet-a", Template: "x", Requires: []string{"nonexistent"}},
		}
		err := ValidateStructure(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), `template_snippets.snippet-a: requires "nonexistent"`)
	})

	t.Run("incremental source must exist and be required", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {
				Name:        "snippet-a",
				Template:    "x",
				Incremental: &IncrementalTemplate{Source: "missing"},
			},
		}
		err := ValidateStructure(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `incremental.source "missing" does not name a watched resource`)

		cfg.TemplateSnippets["snippet-a"] = TemplateSnippet{
			Name:        "snippet-a",
			Template:    "x",
			Incremental: &IncrementalTemplate{Source: "routes"},
		}
		err = ValidateStructure(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `incremental.source "routes" must also appear in requires`)

		cfg.TemplateSnippets["snippet-a"] = TemplateSnippet{
			Name:        "snippet-a",
			Template:    "x",
			Requires:    []string{"routes"},
			Incremental: &IncrementalTemplate{Source: "routes"},
		}
		assert.NoError(t, ValidateStructure(cfg))
	})

	t.Run("incremental source mode is exclusive", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {
				Name:        "snippet-a",
				Template:    "x",
				Incremental: &IncrementalTemplate{},
			},
		}
		err := ValidateStructure(cfg)
		require.ErrorContains(t, err, "incremental requires exactly one of source or bindings_template")

		cfg.TemplateSnippets["snippet-a"] = TemplateSnippet{
			Name:     "snippet-a",
			Template: "x",
			Requires: []string{"routes"},
			Incremental: &IncrementalTemplate{
				Source:           "routes",
				BindingsTemplate: `{% show "{}" %}`,
			},
		}
		err = ValidateStructure(cfg)
		require.ErrorContains(t, err, "incremental requires exactly one of source or bindings_template")
	})

	t.Run("incremental bindings template does not require a static source", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {
				Name:     "snippet-a",
				Template: "x",
				Incremental: &IncrementalTemplate{
					BindingsTemplate: `{% show "{}" %}`,
				},
			},
		}
		assert.NoError(t, ValidateStructure(cfg))
	})

	t.Run("incremental effects are explicit and unique", func(t *testing.T) {
		cfg := base()
		cfg.TemplateSnippets = map[string]TemplateSnippet{
			"snippet-a": {
				Name:     "snippet-a",
				Template: "x",
				Incremental: &IncrementalTemplate{
					BindingsTemplate: `{% show "{}" %}`,
					Effects: []IncrementalEffect{
						IncrementalEffectDeriveResource,
						IncrementalEffectRecordEvent,
						IncrementalEffectBackendPlan,
						IncrementalEffectPublishValue,
					},
				},
			},
		}
		assert.NoError(t, ValidateStructure(cfg))

		incremental := cfg.TemplateSnippets["snippet-a"].Incremental
		incremental.Effects = []IncrementalEffect{"unknown"}
		err := ValidateStructure(cfg)
		require.ErrorContains(t, err, `incremental.effects contains unsupported value "unknown"`)

		incremental.Effects = []IncrementalEffect{
			IncrementalEffectDeriveResource,
			IncrementalEffectDeriveResource,
		}
		err = ValidateStructure(cfg)
		require.ErrorContains(t, err, `incremental.effects contains duplicate value "deriveResource"`)
	})

	t.Run("private incremental prefixes are reserved", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			prefix string
			set    func(*Config, string)
		}{
			{
				name:   "snippet component",
				prefix: IncrementalTemplatePrefix,
				set: func(cfg *Config, name string) {
					cfg.TemplateSnippets = map[string]TemplateSnippet{name: {Name: name, Template: "x"}}
				},
			},
			{
				name:   "map planner",
				prefix: IncrementalBindingsTemplatePrefix,
				set: func(cfg *Config, name string) {
					cfg.Maps = map[string]MapFile{name: {Template: "x"}}
				},
			},
			{
				name:   "file component",
				prefix: IncrementalTemplatePrefix,
				set: func(cfg *Config, name string) {
					cfg.Files = map[string]GeneralFile{name: {Template: "x"}}
				},
			},
			{
				name:   "certificate planner",
				prefix: IncrementalBindingsTemplatePrefix,
				set: func(cfg *Config, name string) {
					cfg.SSLCertificates = map[string]SSLCertificate{name: {Template: "x"}}
				},
			},
			{
				name:   "resource component",
				prefix: IncrementalTemplatePrefix,
				set: func(cfg *Config, name string) {
					cfg.K8sResources = map[string]K8sResource{name: {Template: "x"}}
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				cfg := base()
				name := test.prefix + "collision"
				test.set(cfg, name)
				err := ValidateStructure(cfg)
				require.ErrorContains(t, err, `names starting with "`+test.prefix+`" are reserved`)
			})
		}
	})

	t.Run("dangling test requires is rejected", func(t *testing.T) {
		cfg := base()
		cfg.ValidationTests = map[string]ValidationTest{
			"test-a": {Requires: []string{"nonexistent"}, Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
		}
		err := ValidateStructure(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), `validation_tests.test-a: requires "nonexistent"`)
	})

	t.Run("requiresFields naming a watched resource field is valid", func(t *testing.T) {
		cfg := base()
		cfg.ValidationTests = map[string]ValidationTest{
			"test-a": {RequiresFields: []string{"routes.spec.rules.filters.cors"}, Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
		}
		assert.NoError(t, ValidateStructure(cfg))
	})

	t.Run("requiresFields with dangling first segment is rejected", func(t *testing.T) {
		cfg := base()
		cfg.ValidationTests = map[string]ValidationTest{
			"test-a": {RequiresFields: []string{"nonexistent.spec.rules"}, Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
		}
		err := ValidateStructure(cfg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), `validation_tests.test-a: requiresFields entry "nonexistent.spec.rules"`)
		assert.Contains(t, err.Error(), `"nonexistent"`)
	})

	t.Run("requiresFields without a field path is rejected", func(t *testing.T) {
		for _, entry := range []string{"routes", "routes."} {
			cfg := base()
			cfg.ValidationTests = map[string]ValidationTest{
				"test-a": {RequiresFields: []string{entry}, Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
			}
			err := ValidateStructure(cfg)
			assert.Error(t, err, "entry %q must be rejected", entry)
			assert.Contains(t, err.Error(), `must be of the form`)
		}
	})
}

func TestValidateTemplateStructure(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		want      string
	}{
		{
			name: "valid source and requires need no runtime defaults",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template:    "x",
						Requires:    []string{"routes"},
						Incremental: &IncrementalTemplate{Source: "routes"},
					},
				}
			},
		},
		{
			name: "source must name a watched resource",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template:    "x",
						Requires:    []string{"routes"},
						Incremental: &IncrementalTemplate{Source: "missing"},
					},
				}
			},
			want: `incremental.source "missing" does not name a watched resource`,
		},
		{
			name: "source must appear in requires",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template:    "x",
						Incremental: &IncrementalTemplate{Source: "routes"},
					},
				}
			},
			want: `incremental.source "routes" must also appear in requires`,
		},
		{
			name: "unsupported effect",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template: "x",
						Incremental: &IncrementalTemplate{
							BindingsTemplate: "{}",
							Effects:          []IncrementalEffect{"unknown"},
						},
					},
				}
			},
			want: `incremental.effects contains unsupported value "unknown"`,
		},
		{
			name: "duplicate effect",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template: "x",
						Incremental: &IncrementalTemplate{
							BindingsTemplate: "{}",
							Effects: []IncrementalEffect{
								IncrementalEffectDeriveResource,
								IncrementalEffectDeriveResource,
							},
						},
					},
				}
			},
			want: `incremental.effects contains duplicate value "deriveResource"`,
		},
		{
			name: "empty activation paths",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template: "x",
						Incremental: &IncrementalTemplate{
							BindingsTemplate:  "{}",
							WhenAnyPathExists: []string{},
						},
					},
				}
			},
			want: "incremental.when_any_path_exists must not be empty",
		},
		{
			name: "duplicate activation path",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template: "x",
						Incremental: &IncrementalTemplate{
							BindingsTemplate: "{}",
							WhenAnyPathExists: []string{
								"metadata.name",
								"metadata.name",
							},
						},
					},
				}
			},
			want: `incremental.when_any_path_exists contains duplicate path "metadata.name"`,
		},
		{
			name: "activation cannot guard derivation owner",
			configure: func(cfg *Config) {
				cfg.TemplateSnippets = map[string]TemplateSnippet{
					"component": {
						Template: "x",
						Incremental: &IncrementalTemplate{
							BindingsTemplate:  "{}",
							WhenAnyPathExists: []string{"metadata.name"},
							Effects:           []IncrementalEffect{IncrementalEffectDeriveResource},
						},
					},
				}
			},
			want: "incremental.when_any_path_exists cannot be combined with deriveResource",
		},
		{
			name: "private entry point prefix",
			configure: func(cfg *Config) {
				name := IncrementalTemplatePrefix + "collision"
				cfg.TemplateSnippets = map[string]TemplateSnippet{name: {Template: "x"}}
			},
			want: `names starting with "` + IncrementalTemplatePrefix + `" are reserved`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{
				WatchedResources: map[string]WatchedResource{"routes": {}},
			}
			test.configure(cfg)

			err := ValidateTemplateStructure(cfg)
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestValidateIncrementalResourceProjection(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*IncrementalTemplate)
		wantErr string
	}{
		{name: "valid with publication group"},
		{
			name: "unknown mode",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Mode = "unknown"
			},
			wantErr: `incremental.mode contains unsupported value "unknown"`,
		},
		{
			name: "source",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Source = "routes"
			},
			wantErr: "incremental.resourceProjection cannot set source",
		},
		{
			name: "missing bindings template",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.BindingsTemplate = ""
			},
			wantErr: "incremental.resourceProjection requires bindings_template",
		},
		{
			name: "missing effect",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Effects = nil
			},
			wantErr: "incremental.resourceProjection requires exactly effects [publishValue]",
		},
		{
			name: "different effect",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Effects = []IncrementalEffect{IncrementalEffectRecordEvent}
			},
			wantErr: "incremental.resourceProjection requires exactly effects [publishValue]",
		},
		{
			name: "additional effect",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Effects = append(incremental.Effects, IncrementalEffectBackendPlan)
			},
			wantErr: "incremental.resourceProjection requires exactly effects [publishValue]",
		},
		{
			name: "activation",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.WhenAnyPathExists = []string{"metadata.name"}
			},
			wantErr: "incremental.resourceProjection cannot set when_any_path_exists",
		},
		{
			name: "root",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Root = "projections"
			},
			wantErr: "incremental.resourceProjection cannot set root",
		},
		{
			name: "consumes",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.Consumes = []string{}
			},
			wantErr: "incremental.resourceProjection cannot set consumes",
		},
		{
			name: "optional consumes",
			mutate: func(incremental *IncrementalTemplate) {
				incremental.OptionalConsumes = []string{"optional"}
			},
			wantErr: "incremental.resourceProjection cannot set optional_consumes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			incremental := &IncrementalTemplate{
				Mode:             IncrementalModeResourceProjection,
				BindingsTemplate: "{}",
				Group:            "selected-resources",
				Effects:          []IncrementalEffect{IncrementalEffectPublishValue},
			}
			if test.mutate != nil {
				test.mutate(incremental)
			}
			cfg := &Config{
				WatchedResources: map[string]WatchedResource{"routes": {}},
				TemplateSnippets: map[string]TemplateSnippet{
					"projection": {
						Template:    `{{- "" -}}`,
						Requires:    []string{"routes"},
						Incremental: incremental,
					},
				},
			}

			err := ValidateTemplateStructure(cfg)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestValidateIncrementalDependencies(t *testing.T) {
	component := func(group string, effects []IncrementalEffect, consumes, optional []string) TemplateSnippet {
		return TemplateSnippet{
			Template: "x",
			Incremental: &IncrementalTemplate{
				BindingsTemplate: "{}",
				Group:            group,
				Consumes:         consumes,
				OptionalConsumes: optional,
				Effects:          effects,
			},
		}
	}
	tests := []struct {
		name     string
		snippets map[string]TemplateSnippet
		absent   map[string]struct{}
		wantErr  string
	}{
		{
			name: "required and optional producers",
			snippets: map[string]TemplateSnippet{
				"policy":   component("policies", []IncrementalEffect{IncrementalEffectPublishValue}, nil, nil),
				"optional": component("optional-policies", []IncrementalEffect{IncrementalEffectPublishValue}, nil, nil),
				"route":    component("routes", nil, []string{"policies"}, []string{"optional-policies"}),
			},
		},
		{
			name: "required group missing",
			snippets: map[string]TemplateSnippet{
				"route": component("routes", nil, []string{"policies"}, nil),
			},
			wantErr: `incremental.consumes "policies" does not name an incremental group`,
		},
		{
			name: "unauthenticated optional group missing",
			snippets: map[string]TemplateSnippet{
				"route": component("routes", nil, nil, []string{"policies"}),
			},
			wantErr: `incremental.optional_consumes "policies" does not name an incremental group`,
		},
		{
			name: "authenticated optional group missing",
			snippets: map[string]TemplateSnippet{
				"route": component("routes", nil, nil, []string{"policies"}),
			},
			absent: map[string]struct{}{"policies": {}},
		},
		{
			name: "target has no publisher",
			snippets: map[string]TemplateSnippet{
				"policy": component("policies", nil, nil, nil),
				"route":  component("routes", nil, []string{"policies"}, nil),
			},
			wantErr: `incremental.consumes group "policies" has no publishValue component`,
		},
		{
			name: "duplicate across fields",
			snippets: map[string]TemplateSnippet{
				"policy": component("policies", []IncrementalEffect{IncrementalEffectPublishValue}, nil, nil),
				"route":  component("routes", nil, []string{"policies"}, []string{"policies"}),
			},
			wantErr: `incremental.optional_consumes contains group "policies" already declared in consumes`,
		},
		{
			name: "self dependency",
			snippets: map[string]TemplateSnippet{
				"route": component("routes", []IncrementalEffect{IncrementalEffectPublishValue}, []string{"routes"}, nil),
			},
			wantErr: `incremental.consumes group "routes" depends on itself`,
		},
		{
			name: "transitive cycle",
			snippets: map[string]TemplateSnippet{
				"a": component("a", []IncrementalEffect{IncrementalEffectPublishValue}, []string{"b"}, nil),
				"b": component("b", []IncrementalEffect{IncrementalEffectPublishValue}, nil, []string{"c"}),
				"c": component("c", []IncrementalEffect{IncrementalEffectPublishValue}, []string{"a"}, nil),
			},
			wantErr: `incremental group dependency cycle: a -> b -> c -> a`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{
				TemplateSnippets:        test.snippets,
				AbsentIncrementalGroups: test.absent,
			}
			err := ValidateTemplateStructure(cfg)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestValidateIncrementalRoots(t *testing.T) {
	component := func(root, source, bindings string) TemplateSnippet {
		requires := []string(nil)
		if source != "" {
			requires = []string{source}
		}
		return TemplateSnippet{
			Template: "x",
			Requires: requires,
			Incremental: &IncrementalTemplate{
				Root: root, Source: source, BindingsTemplate: bindings,
			},
		}
	}
	tests := []struct {
		name     string
		snippets map[string]TemplateSnippet
		wantErr  string
	}{
		{
			name: "same source",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "routes", ""),
				"b": component("routes", "routes", ""),
			},
		},
		{
			name: "same bindings",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "", `{% show bindings %}`),
				"b": component("routes", "", `{% show bindings %}`),
			},
		},
		{
			name: "different source",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "routes", ""),
				"b": component("routes", "services", ""),
			},
		},
		{
			name: "different bindings",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "", "first"),
				"b": component("routes", "", "second"),
			},
		},
		{
			name: "mixed identity modes",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "routes", ""),
				"b": component("routes", "", "bindings"),
			},
		},
		{
			name: "surrounding whitespace",
			snippets: map[string]TemplateSnippet{
				"a": component(" routes", "routes", ""),
			},
			wantErr: "incremental.root must not contain surrounding whitespace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{
				WatchedResources: map[string]WatchedResource{
					"routes": {Resources: "routes"}, "services": {Resources: "services"},
				},
				TemplateSnippets: test.snippets,
			}
			err := ValidateTemplateStructure(cfg)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestValidateIncrementalRootBarriers(t *testing.T) {
	component := func(root, group string, consumes, optional []string, publish bool) TemplateSnippet {
		effects := []IncrementalEffect(nil)
		if publish {
			effects = []IncrementalEffect{IncrementalEffectPublishValue}
		}
		return TemplateSnippet{
			Template: "x",
			Incremental: &IncrementalTemplate{
				Root: root, BindingsTemplate: "{}", Group: group,
				Consumes: consumes, OptionalConsumes: optional, Effects: effects,
			},
		}
	}
	tests := []struct {
		name     string
		snippets map[string]TemplateSnippet
		wantErr  string
	}{
		{
			name: "independent groups",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "a", nil, nil, false),
				"b": component("routes", "b", nil, nil, false),
			},
		},
		{
			name: "dependency outside root",
			snippets: map[string]TemplateSnippet{
				"producer": component("", "producer", nil, nil, true),
				"consumer": component("routes", "consumer", []string{"producer"}, nil, false),
			},
		},
		{
			name: "direct dependency",
			snippets: map[string]TemplateSnippet{
				"producer": component("routes", "producer", nil, nil, true),
				"consumer": component("routes", "consumer", []string{"producer"}, nil, false),
			},
			wantErr: `incremental.root "routes" crosses a dependency barrier: consumer -> producer`,
		},
		{
			name: "optional dependency",
			snippets: map[string]TemplateSnippet{
				"producer": component("routes", "producer", nil, nil, true),
				"consumer": component("routes", "consumer", nil, []string{"producer"}, false),
			},
			wantErr: `incremental.root "routes" crosses a dependency barrier: consumer -> producer`,
		},
		{
			name: "transitive dependency",
			snippets: map[string]TemplateSnippet{
				"a": component("routes", "a", []string{"b"}, nil, false),
				"b": component("", "b", []string{"c"}, nil, true),
				"c": component("routes", "c", nil, nil, true),
			},
			wantErr: `incremental.root "routes" crosses a dependency barrier: a -> b -> c`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateTemplateStructure(&Config{TemplateSnippets: test.snippets})
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestWatchedResource_CandidateVersions(t *testing.T) {
	list := WatchedResource{APIVersions: []string{"example.io/v1", "example.io/v1beta1"}}
	assert.Equal(t, []string{"example.io/v1", "example.io/v1beta1"}, list.CandidateVersions())

	single := WatchedResource{APIVersion: "example.io/v1"}
	assert.Equal(t, []string{"example.io/v1"}, single.CandidateVersions())
}

// mergedCompletenessConfig is the minimum a merged config must satisfy: the
// fields whose CRD `Required` markers were dropped so a single object of a
// merged set can be incomplete (ADR-0014). Deliberately carries none of the
// dataplane runtime defaults — ValidateMergedCompleteness must not demand them,
// or the admission webhook would start denying hand-written configs.
func mergedCompletenessConfig() *Config {
	return &Config{
		PodSelector:      PodSelector{MatchLabels: map[string]string{"app": "haproxy"}},
		WatchedResources: map[string]WatchedResource{"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace"}}},
		HAProxyConfig:    HAProxyConfig{Template: "global"},
	}
}

func TestValidateMergedCompleteness(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "a complete merged config with no dataplane defaults passes",
			mutate: func(*Config) {},
		},
		{
			name:    "podSelector is required after the merge",
			mutate:  func(c *Config) { c.PodSelector = PodSelector{} },
			wantErr: "pod_selector: match_labels cannot be empty",
		},
		{
			name:    "at least one watched resource is required after the merge",
			mutate:  func(c *Config) { c.WatchedResources = nil },
			wantErr: "watched_resources: at least one resource must be configured",
		},
		{
			name:    "the haproxy template is required after the merge",
			mutate:  func(c *Config) { c.HAProxyConfig = HAProxyConfig{} },
			wantErr: "haproxy_config: template cannot be empty",
		},
		{
			name: "a test that asserts nothing is rejected",
			mutate: func(c *Config) {
				c.ValidationTests = map[string]ValidationTest{"silent": {}}
			},
			wantErr: "validation_tests.silent: must declare at least one assertion",
		},
		{
			name: "a test with an assertion is accepted",
			mutate: func(c *Config) {
				c.ValidationTests = map[string]ValidationTest{
					"real": {Assertions: []ValidationAssertion{{Type: "haproxy_valid"}}},
				}
			},
		},
		{
			// _global is a shared baseline, not a test — the runner never
			// executes its assertions, and several libraries each contribute
			// part of it, so their objects carry an incomplete one.
			name: "the _global baseline may assert nothing",
			mutate: func(c *Config) {
				c.ValidationTests = map[string]ValidationTest{
					GlobalValidationTestName: {Fixtures: map[string][]any{}},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mergedCompletenessConfig()
			tt.mutate(cfg)

			err := ValidateMergedCompleteness(cfg)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// The load-path gate must keep enforcing everything the narrower prospective
// gate does, so a config the webhook admits can't fail differently on load.
func TestValidateStructure_SubsumesMergedCompleteness(t *testing.T) {
	cfg := mergedCompletenessConfig()
	cfg.Logging = LoggingConfig{Level: "INFO"}
	cfg.Dataplane = DataplaneConfig{
		Port: 5555, MapsDir: "/etc/haproxy/maps", SSLCertsDir: "/etc/haproxy/certs",
		GeneralStorageDir: "/etc/haproxy/general", SecretsDir: "/etc/haproxy/secrets", ConfigFile: "/etc/haproxy/haproxy.cfg",
	}
	cfg.ValidationTests = map[string]ValidationTest{"silent": {}}

	assert.ErrorContains(t, ValidateStructure(cfg),
		"validation_tests.silent: must declare at least one assertion")
}

func TestValidationTestsRejectGlobalDeploymentWatchScopes(t *testing.T) {
	err := validateValidationTests(&Config{ValidationTests: map[string]ValidationTest{GlobalValidationTestName: {UseDeploymentWatchScopes: true}}})
	require.ErrorContains(t, err, "validationTests._global.useDeploymentWatchScopes")
}
