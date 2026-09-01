// Package config parses the plugin's own YAML config subtree.
//
// The host sends the complete preserved `plugins.configs.reset-priority`
// subtree as the `config_yaml` field of plugin.register / plugin.reconfigure
// (a []byte, so base64 on the wire; the ABI layer decodes it before this
// package sees plain YAML bytes).
package config

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults for every tunable.
const (
	DefaultReconcileInterval  = time.Hour
	DefaultRequestTimeout     = 10 * time.Second
	DefaultPriorityFloor      = 100
	DefaultPriorityStep       = 100
	DefaultQuarantinePriority = 0
)

// Provider IDs managed by this plugin.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// Config is the validated plugin configuration.
type Config struct {
	// Enabled mirrors the per-plugin enabled flag the host preserves in the
	// subtree. The host normally does not activate disabled plugins at all;
	// this is a defensive second gate.
	Enabled bool
	// ReconcileInterval is the background reconciliation interval
	// (`reconcile-interval`, alias `refresh-interval`). It is a safety net,
	// not the reset-time precision: exact deadline timers fire independently.
	ReconcileInterval time.Duration
	// RequestTimeout bounds each provider quota HTTP request.
	RequestTimeout time.Duration
	// PriorityFloor is the priority of the latest-resetting healthy account.
	PriorityFloor int
	// PriorityStep is the gap between adjacent ranks.
	PriorityStep int
	// QuarantinePriority is the sentinel priority for managed accounts that
	// are outside the active pool (disabled / reauth-required / recovering).
	QuarantinePriority int
	// ManageClaude / ManageCodex select the managed provider groups.
	ManageClaude bool
	ManageCodex  bool
	// DryRun computes and reports desired priorities but never calls
	// host.auth.save.
	DryRun bool
	// Warnings carries sanitized, non-fatal parse notes for status display.
	Warnings []string
}

// ManagedProviders returns the enabled provider IDs in a fixed order.
func (c Config) ManagedProviders() []string {
	out := make([]string, 0, 2)
	if c.ManageClaude {
		out = append(out, ProviderClaude)
	}
	if c.ManageCodex {
		out = append(out, ProviderCodex)
	}
	return out
}

// rawConfig models the YAML subtree. Pointers distinguish absent from zero.
// The `priority` key is the CPA plugin load/order priority and is
// intentionally ignored here: it is unrelated to the credential priorities
// this plugin manages.
type rawConfig struct {
	Enabled           *bool      `yaml:"enabled"`
	LoadPriority      any        `yaml:"priority"` // host-owned; ignored
	ReconcileInterval *duration  `yaml:"reconcile-interval"`
	RefreshInterval   *duration  `yaml:"refresh-interval"` // alias
	RequestTimeout    *duration  `yaml:"request-timeout"`
	PriorityFloor     *strictInt `yaml:"priority-floor"`
	PriorityStep      *strictInt `yaml:"priority-step"`
	Quarantine        *strictInt `yaml:"quarantine-priority"`
	ManageClaude      *bool      `yaml:"manage-claude"`
	ManageCodex       *bool      `yaml:"manage-codex"`
	Providers         *[]string  `yaml:"providers"` // alias for manage-*
	DryRun            *bool      `yaml:"dry-run"`
	// codex-reset-window-activation is a documented phase-2 option. v0.1.0
	// deliberately does not implement active Codex reset-window activation
	// (it can consume quota); a true value is accepted but ignored with a
	// warning so operators are not surprised.
	CodexActivation *bool `yaml:"codex-reset-window-activation"`
}

// duration accepts Go duration strings ("1h", "10s") and bare YAML integers
// (seconds). Dispatch is on the resolved YAML tag: yaml.v3 silently
// truncates floats when decoding into integer types ("90.5" -> 90 seconds),
// so non-integer numeric scalars are rejected instead of truncated.
type duration struct{ d time.Duration }

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!int":
		var asInt int64
		if err := node.Decode(&asInt); err != nil {
			return fmt.Errorf("invalid duration integer")
		}
		const (
			maxSeconds = int64(math.MaxInt64) / int64(time.Second)
			minSeconds = int64(math.MinInt64) / int64(time.Second)
		)
		if asInt > maxSeconds || asInt < minSeconds {
			return fmt.Errorf("duration seconds overflow time.Duration")
		}
		d.d = time.Duration(asInt) * time.Second
		return nil
	case "!!str":
		var asString string
		if err := node.Decode(&asString); err != nil {
			return fmt.Errorf("invalid duration value")
		}
		parsed, errParse := time.ParseDuration(strings.TrimSpace(asString))
		if errParse != nil {
			return fmt.Errorf("invalid duration %q", asString)
		}
		d.d = parsed
		return nil
	case "!!float":
		return fmt.Errorf("duration must be whole seconds or a duration string like \"90s\"; float values are rejected to avoid truncation")
	default:
		return fmt.Errorf("invalid duration value")
	}
}

// strictInt accepts only true YAML integers. yaml.v3 silently truncates
// floats when decoding into Go int fields ("100.5" -> 100), which would let
// a config typo shift credential priorities; any non-integer scalar is
// rejected instead.
type strictInt struct{ v int }

func (s *strictInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return fmt.Errorf("value must be an integer; float values are rejected to avoid truncation")
	}
	if err := node.Decode(&s.v); err != nil {
		return fmt.Errorf("invalid integer value")
	}
	return nil
}

// Parse decodes and validates the plugin config subtree. A nil/empty subtree
// yields defaults with Enabled=false (matching the host's default runtime
// config for unconfigured plugins).
func Parse(configYAML []byte) (Config, error) {
	cfg := Config{
		Enabled:            false,
		ReconcileInterval:  DefaultReconcileInterval,
		RequestTimeout:     DefaultRequestTimeout,
		PriorityFloor:      DefaultPriorityFloor,
		PriorityStep:       DefaultPriorityStep,
		QuarantinePriority: DefaultQuarantinePriority,
		ManageClaude:       true,
		ManageCodex:        true,
		DryRun:             false,
	}
	trimmed := strings.TrimSpace(string(configYAML))
	if trimmed == "" {
		return cfg, nil
	}

	var raw rawConfig
	if err := yaml.Unmarshal(configYAML, &raw); err != nil {
		return Config{}, fmt.Errorf("parse reset-priority config: %w", err)
	}

	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.ReconcileInterval != nil {
		cfg.ReconcileInterval = raw.ReconcileInterval.d
	} else if raw.RefreshInterval != nil {
		cfg.ReconcileInterval = raw.RefreshInterval.d
	}
	if raw.RequestTimeout != nil {
		cfg.RequestTimeout = raw.RequestTimeout.d
	}
	if raw.PriorityFloor != nil {
		cfg.PriorityFloor = raw.PriorityFloor.v
	}
	if raw.PriorityStep != nil {
		cfg.PriorityStep = raw.PriorityStep.v
	}
	if raw.Quarantine != nil {
		cfg.QuarantinePriority = raw.Quarantine.v
	}
	if raw.Providers != nil {
		cfg.ManageClaude = false
		cfg.ManageCodex = false
		for _, p := range *raw.Providers {
			switch strings.ToLower(strings.TrimSpace(p)) {
			case ProviderClaude:
				cfg.ManageClaude = true
			case ProviderCodex:
				cfg.ManageCodex = true
			default:
				return Config{}, fmt.Errorf("unsupported provider %q: only %q and %q are managed", p, ProviderClaude, ProviderCodex)
			}
		}
	}
	// Explicit manage-* flags win over the providers alias.
	if raw.ManageClaude != nil {
		cfg.ManageClaude = *raw.ManageClaude
	}
	if raw.ManageCodex != nil {
		cfg.ManageCodex = *raw.ManageCodex
	}
	if raw.DryRun != nil {
		cfg.DryRun = *raw.DryRun
	}
	if raw.CodexActivation != nil && *raw.CodexActivation {
		cfg.Warnings = append(cfg.Warnings,
			"codex-reset-window-activation is not implemented in v0.1.0 and is ignored; accounts in awaiting_new_window recover via passive re-reads only")
	}

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg Config) error {
	if cfg.ReconcileInterval <= 0 {
		return fmt.Errorf("reconcile-interval must be positive")
	}
	if cfg.RequestTimeout <= 0 {
		return fmt.Errorf("request-timeout must be positive")
	}
	if cfg.PriorityStep <= 0 {
		return fmt.Errorf("priority-step must be positive")
	}
	if cfg.PriorityFloor <= 0 {
		return fmt.Errorf("priority-floor must be positive")
	}
	if cfg.QuarantinePriority < 0 {
		return fmt.Errorf("quarantine-priority must not be negative")
	}
	if cfg.QuarantinePriority >= cfg.PriorityFloor {
		return fmt.Errorf("quarantine-priority (%d) must be below priority-floor (%d)", cfg.QuarantinePriority, cfg.PriorityFloor)
	}
	return nil
}
