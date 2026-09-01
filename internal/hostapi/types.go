// Package hostapi defines the JSON wire contract between this plugin and the
// CLIProxyAPI plugin host, plus a typed bridge over the raw host-callback
// function.
//
// The shapes below were audited against CLIProxyAPI commit
// 81e1b5374f99c212f196f34956eeed964a46b8fa (nearest release v7.2.146):
//
//   - Native ABI version 1, JSON RPC schema version 4.
//   - Lifecycle requests carry `config_yaml` as a Go []byte, which standard
//     encoding/json transports as base64 text.
//   - Registration metadata (`pluginapi.Metadata`) has no JSON tags upstream,
//     so its wire keys are the capitalized Go field names.
//   - Management route/request/response types likewise have no JSON tags
//     upstream (keys `Method`, `Path`, `Menu`, `Description`, `StatusCode`,
//     `Headers`, `Body`, `Query`), while the registration wrapper uses
//     lowercase `routes` / `resources` and capabilities use snake_case.
//
// These types are written independently against that wire contract; no
// upstream implementation code is copied.
package hostapi

import (
	"encoding/json"
	"time"
)

// ABI / RPC schema constants negotiated with the host.
const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 4
)

// Lifecycle and capability RPC method names (host -> plugin).
const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginQuiesce     = "plugin.quiesce"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

// Host callback method names (plugin -> host).
const (
	MethodHostAuthList       = "host.auth.list"
	MethodHostAuthGet        = "host.auth.get"
	MethodHostAuthGetRuntime = "host.auth.get_runtime"
	MethodHostAuthSave       = "host.auth.save"
	MethodHostHTTPDo         = "host.http.do"
	MethodHostLog            = "host.log"
)

// Envelope is the common RPC result wrapper used in both directions.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a structured RPC failure.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// LifecycleRequest is the plugin.register / plugin.reconfigure request body.
// ConfigYAML is base64 on the wire because it is a []byte in the host's
// request struct; encoding/json decodes it transparently.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Metadata mirrors upstream pluginapi.Metadata, whose fields have no JSON
// tags; the wire keys are therefore the capitalized Go names.
type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

// ConfigField mirrors upstream pluginapi.ConfigField (no JSON tags upstream).
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

// Registration is the plugin.register / plugin.reconfigure result.
type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Capabilities declares only the capability this plugin implements.
// Capability keys are snake_case on the wire.
type Capabilities struct {
	ManagementAPI bool `json:"management_api"`
}

// ManagementRegistration is the management.register result. The wrapper keys
// are lowercase; the route fields are capitalized (untagged upstream).
type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute declares one authenticated Management API route. GET routes
// with a non-empty Menu are treated by the host as legacy resource routes, so
// authenticated routes must leave Menu empty.
type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute declares one unauthenticated, GET-only browser resource under
// /v0/resource/plugins/<pluginID>/. It must never perform mutations.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ManagementRequest is the management.handle request (untagged upstream, so
// capitalized keys; Body is base64 []byte on the wire).
type ManagementRequest struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        map[string][]string `json:"Headers"`
	Query          map[string][]string `json:"Query"`
	Body           []byte              `json:"Body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// ManagementResponse is the management.handle result (untagged upstream).
type ManagementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// AuthEntry mirrors the relevant subset of upstream pluginapi.HostAuthFileEntry
// returned by host.auth.list and host.auth.get_runtime.
//
// Note: Priority uses omitempty upstream, so a missing key can mean either
// "no priority field" or "priority zero"; exact physical state requires
// host.auth.get.
type AuthEntry struct {
	ID            string    `json:"id,omitempty"`
	AuthIndex     string    `json:"auth_index,omitempty"`
	Name          string    `json:"name"`
	Type          string    `json:"type,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Label         string    `json:"label,omitempty"`
	Status        string    `json:"status,omitempty"`
	StatusMessage string    `json:"status_message,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
	Unavailable   bool      `json:"unavailable,omitempty"`
	RuntimeOnly   bool      `json:"runtime_only,omitempty"`
	Source        string    `json:"source,omitempty"`
	Path          string    `json:"path,omitempty"`
	Email         string    `json:"email,omitempty"`
	Priority      int       `json:"priority,omitempty"`
	LastRefresh   time.Time `json:"last_refresh,omitempty"`
	ModTime       time.Time `json:"mod_time,omitempty"`
}

// AuthListResponse is the host.auth.list result.
type AuthListResponse struct {
	Files []AuthEntry `json:"files"`
}

// AuthGetRequest is the host.auth.get / host.auth.get_runtime request (both
// audited callbacks take the same single auth_index field).
type AuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

// AuthGetRuntimeResponse is the host.auth.get_runtime result. The audited
// host resolves the index against the in-memory auth manager and returns the
// same untrusted-metadata entry shape as host.auth.list, wrapped in an "auth"
// key. It carries runtime health only (status, status_message, disabled,
// unavailable, ...) and never credential JSON. Audited caveats:
//   - the host returns an RPC error, not an empty entry, when the auth index
//     is unknown, when the auth is runtime-only and disabled, or when a
//     disabled/removed file-backed auth's physical file has disappeared;
//   - Priority uses omitempty upstream, so zero remains ambiguous exactly as
//     in list entries and must not be treated as an exact physical value.
type AuthGetRuntimeResponse struct {
	Auth AuthEntry `json:"auth"`
}

// AuthGetResponse is the host.auth.get result carrying the complete physical
// credential JSON. The JSON payload contains secrets and must never be
// logged or exposed.
type AuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// AuthSaveRequest is the host.auth.save request. Save replaces the complete
// physical JSON document identified by Name; there is no field-patch
// callback, so callers must re-read and preserve every unrelated field.
type AuthSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

// AuthSaveResponse is the host.auth.save result.
type AuthSaveResponse struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// HTTPRequest is the host.http.do request (snake_case keys; Body base64).
type HTTPRequest struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// HTTPResponse is the host.http.do result. Upstream returns the untagged
// pluginapi.HTTPResponse, so the wire keys are capitalized and Body is
// base64.
type HTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// LogRequest is the host.log request.
type LogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}
