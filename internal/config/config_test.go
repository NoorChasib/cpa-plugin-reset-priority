package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseEmptyYieldsDefaultsDisabled(t *testing.T) {
	for _, raw := range []string{"", "   \n"} {
		cfg, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if cfg.Enabled {
			t.Errorf("empty config must not be enabled")
		}
		assertDefaults(t, cfg)
	}
}

func TestParseHostDefaultSubtree(t *testing.T) {
	// The host sends "enabled: false\npriority: 0\n" for unconfigured
	// plugins.
	cfg, err := Parse([]byte("enabled: false\npriority: 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Enabled {
		t.Errorf("enabled = true, want false")
	}
	assertDefaults(t, cfg)
}

func assertDefaults(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.ReconcileInterval != time.Hour {
		t.Errorf("ReconcileInterval = %s, want 1h", cfg.ReconcileInterval)
	}
	if cfg.RequestTimeout != 10*time.Second {
		t.Errorf("RequestTimeout = %s, want 10s", cfg.RequestTimeout)
	}
	if cfg.PriorityFloor != 100 || cfg.PriorityStep != 100 {
		t.Errorf("floor/step = %d/%d, want 100/100", cfg.PriorityFloor, cfg.PriorityStep)
	}
	if cfg.QuarantinePriority != 0 {
		t.Errorf("QuarantinePriority = %d, want 0", cfg.QuarantinePriority)
	}
	if !cfg.ManageClaude || !cfg.ManageCodex {
		t.Errorf("manage flags = %t/%t, want true/true", cfg.ManageClaude, cfg.ManageCodex)
	}
	if cfg.DryRun {
		t.Errorf("DryRun = true, want false")
	}
}

func TestParseFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
enabled: true
priority: 10
reconcile-interval: 30m
request-timeout: 5s
priority-floor: 50
priority-step: 25
quarantine-priority: 5
manage-claude: true
manage-codex: false
dry-run: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Enabled || !cfg.DryRun {
		t.Errorf("enabled/dry-run = %t/%t, want true/true", cfg.Enabled, cfg.DryRun)
	}
	if cfg.ReconcileInterval != 30*time.Minute || cfg.RequestTimeout != 5*time.Second {
		t.Errorf("durations = %s/%s", cfg.ReconcileInterval, cfg.RequestTimeout)
	}
	if cfg.PriorityFloor != 50 || cfg.PriorityStep != 25 || cfg.QuarantinePriority != 5 {
		t.Errorf("floor/step/quarantine = %d/%d/%d", cfg.PriorityFloor, cfg.PriorityStep, cfg.QuarantinePriority)
	}
	if !cfg.ManageClaude || cfg.ManageCodex {
		t.Errorf("manage = %t/%t, want true/false", cfg.ManageClaude, cfg.ManageCodex)
	}
	if !cfg.Manages(ProviderClaude) || cfg.Manages(ProviderCodex) || cfg.Manages("other") {
		t.Errorf("Manages does not match configured provider flags")
	}
}

func TestParseAliases(t *testing.T) {
	cfg, err := Parse([]byte(`
enabled: true
refresh-interval: 45m
providers:
  - codex
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ReconcileInterval != 45*time.Minute {
		t.Errorf("refresh-interval alias not applied: %s", cfg.ReconcileInterval)
	}
	if cfg.ManageClaude || !cfg.ManageCodex {
		t.Errorf("providers alias = %t/%t, want false/true", cfg.ManageClaude, cfg.ManageCodex)
	}

	// reconcile-interval wins over the alias; manage-* wins over providers.
	cfg, err = Parse([]byte(`
enabled: true
reconcile-interval: 20m
refresh-interval: 45m
providers: [codex]
manage-claude: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ReconcileInterval != 20*time.Minute {
		t.Errorf("reconcile-interval should win: %s", cfg.ReconcileInterval)
	}
	if !cfg.ManageClaude || !cfg.ManageCodex {
		t.Errorf("manage flags = %t/%t, want true/true", cfg.ManageClaude, cfg.ManageCodex)
	}
}

func TestParseIntegerSecondsDuration(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\nrequest-timeout: 30\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %s, want 30s", cfg.RequestTimeout)
	}
}

func TestIntegerSecondsDurationOverflowBoundaries(t *testing.T) {
	const (
		maxSafeSeconds = int64(9223372036)
		minSafeSeconds = int64(-9223372036)
	)
	for name, tc := range map[string]struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		"maximum safe":      {raw: "9223372036", want: time.Duration(maxSafeSeconds) * time.Second},
		"positive overflow": {raw: "9223372037", wantErr: true},
		"minimum safe":      {raw: "-9223372036", want: time.Duration(minSafeSeconds) * time.Second},
		"negative overflow": {raw: "-9223372037", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			var got duration
			err := yaml.Unmarshal([]byte(tc.raw), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("yaml.Unmarshal(%q) accepted overflowing integer seconds", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("yaml.Unmarshal(%q): %v", tc.raw, err)
			}
			if got.d != tc.want {
				t.Errorf("duration = %d, want %d", got.d, tc.want)
			}
		})
	}
}

func TestDurationRejectsFloatsWithoutTruncation(t *testing.T) {
	// yaml.v3 silently truncates floats into integer targets ("90.5" -> 90);
	// duration fields must reject them instead.
	for name, snippet := range map[string]string{
		"fractional seconds":       "request-timeout: 90.5",
		"integral float seconds":   "request-timeout: 90.0",
		"exponent seconds":         "request-timeout: 1.5e3",
		"explicit float tag":       "request-timeout: !!float 90",
		"reconcile fractional":     "reconcile-interval: 0.5",
		"refresh alias fractional": "refresh-interval: 30.75",
		"sequence value":           "request-timeout: [10]",
		"mapping value":            "request-timeout: {seconds: 10}",
		"boolean value":            "request-timeout: true",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte("enabled: true\n" + snippet + "\n")); err == nil {
				t.Errorf("Parse accepted %q, want float/non-integer rejection", snippet)
			}
		})
	}
	// Integer seconds and duration strings still work.
	cfg, err := Parse([]byte("enabled: true\nrequest-timeout: 90\nreconcile-interval: 30m\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RequestTimeout != 90*time.Second || cfg.ReconcileInterval != 30*time.Minute {
		t.Errorf("durations = %s/%s", cfg.RequestTimeout, cfg.ReconcileInterval)
	}
	// An explicit null leaves the pointer nil and is treated as absent, so
	// the default applies (no custom unmarshal runs for !!null).
	cfg, err = Parse([]byte("enabled: true\nrequest-timeout: null\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("null request-timeout = %s, want default %s", cfg.RequestTimeout, DefaultRequestTimeout)
	}
}

func TestPriorityIntegersRejectFloatsWithoutTruncation(t *testing.T) {
	for name, snippet := range map[string]string{
		"floor fractional":       "priority-floor: 100.5",
		"floor integral float":   "priority-floor: 100.0",
		"floor exponent":         "priority-floor: 1e2",
		"step fractional":        "priority-step: 25.5",
		"quarantine fractional":  "quarantine-priority: 0.5",
		"floor string":           `priority-floor: "100"`,
		"floor boolean":          "priority-floor: true",
		"floor sequence":         "priority-floor: [100]",
		"floor int overflow":     "priority-floor: 99999999999999999999",
		"step negative overflow": "priority-step: -99999999999999999999",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte("enabled: true\n" + snippet + "\n")); err == nil {
				t.Errorf("Parse accepted %q, want float/non-integer rejection", snippet)
			}
		})
	}
	// Plain integers still work (also covered by TestParseFullConfig).
	cfg, err := Parse([]byte("enabled: true\npriority-floor: 200\npriority-step: 50\nquarantine-priority: 10\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PriorityFloor != 200 || cfg.PriorityStep != 50 || cfg.QuarantinePriority != 10 {
		t.Errorf("floor/step/quarantine = %d/%d/%d", cfg.PriorityFloor, cfg.PriorityStep, cfg.QuarantinePriority)
	}
}

func TestParseValidationErrors(t *testing.T) {
	cases := map[string]string{
		"zero step":           "priority-step: 0",
		"negative step":       "priority-step: -100",
		"zero floor":          "priority-floor: 0",
		"negative quarantine": "quarantine-priority: -1",
		"quarantine >= floor": "quarantine-priority: 100",
		"zero interval":       "reconcile-interval: 0s",
		"negative timeout":    "request-timeout: -5s",
		"bad duration":        "reconcile-interval: soon",
		"unknown provider":    "providers: [gemini]",
		"malformed yaml":      "enabled: [",
	}
	for name, snippet := range cases {
		if _, err := Parse([]byte("enabled: true\n" + snippet + "\n")); err == nil {
			t.Errorf("%s: Parse accepted %q, want error", name, snippet)
		}
	}
}

func TestParseCodexActivationIgnoredWithWarning(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\ncodex-reset-window-activation: true\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "not implemented") {
		t.Errorf("warnings = %v, want a not-implemented warning", cfg.Warnings)
	}

	// Explicit false produces no warning.
	cfg, err = Parse([]byte("enabled: true\ncodex-reset-window-activation: false\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", cfg.Warnings)
	}
}

func TestParseUnknownFieldsIgnored(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\nfuture-option: whatever\nstore:\n  version: 1.2.3\n"))
	if err != nil {
		t.Fatalf("unknown fields must be tolerated: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("enabled lost while parsing unknown fields")
	}
}

func TestParseLoadPriorityIsIgnored(t *testing.T) {
	// plugins.configs.reset-priority.priority is the CPA plugin LOAD
	// priority; it must not affect any credential priority setting.
	cfg, err := Parse([]byte("enabled: true\npriority: 999\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PriorityFloor != 100 || cfg.PriorityStep != 100 {
		t.Errorf("plugin load priority leaked into floor/step: %d/%d", cfg.PriorityFloor, cfg.PriorityStep)
	}
}
