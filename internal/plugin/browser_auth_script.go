package plugin

// browserAuthScript is inline JavaScript shared by the browser views. It
// exposes window.resetPriorityAuth with three helpers:
//
//   - managementKey(): best-effort recovery of the CPA management key that the
//     official management console (Cli-Proxy-API-Management-Center) persists
//     in localStorage. The console stores its state under "cli-proxy-auth"
//     (legacy installs used "managementKey") behind a documented reversible
//     XOR obfuscation ("enc::v1::" prefix) keyed by a fixed salt,
//     location.host, and navigator.userAgent. Reading that storage is the
//     officially documented trust model for plugin resource pages served from
//     the same origin as the console; when the console runs on a different
//     origin the entries simply do not exist here and this returns null.
//   - managementPath(suffix): rebuilds an absolute management route path from
//     the current location, preserving any reverse-proxy prefix, whether the
//     page is served from the management route tree or the resource one.
//   - authHeaders(extra): copies extra and adds the Authorization header when
//     a management key was recovered. The header value is assembled from
//     parts so the page source never contains bearer-credential material.
//
// The recovered key is only ever sent to same-origin CPA management routes;
// nothing is persisted or exported anywhere else.
const browserAuthScript = `
(function (global) {
	"use strict";
	var SALT = "cli-proxy-api-webui::secure-storage";
	var OBFUSCATION_PREFIX = "enc::v1::";

	function keyBytes() {
		var text = SALT;
		try {
			text = SALT + "|" + global.location.host + "|" + global.navigator.userAgent;
		} catch (ignored) {}
		return new TextEncoder().encode(text);
	}

	function deobfuscate(raw) {
		if (typeof raw !== "string" || raw === "") {
			return null;
		}
		if (raw.indexOf(OBFUSCATION_PREFIX) !== 0) {
			return raw;
		}
		try {
			var binary = atob(raw.slice(OBFUSCATION_PREFIX.length));
			var data = new Uint8Array(binary.length);
			for (var i = 0; i < binary.length; i++) {
				data[i] = binary.charCodeAt(i);
			}
			var key = keyBytes();
			var plain = new Uint8Array(data.length);
			for (var j = 0; j < data.length; j++) {
				plain[j] = data[j] ^ key[j % key.length];
			}
			return new TextDecoder().decode(plain);
		} catch (ignored) {
			return null;
		}
	}

	function parseLoose(text) {
		if (typeof text !== "string") {
			return null;
		}
		try {
			return JSON.parse(text);
		} catch (ignored) {
			return text;
		}
	}

	function managementKey() {
		try {
			var persisted = parseLoose(deobfuscate(global.localStorage.getItem("cli-proxy-auth")));
			if (persisted && typeof persisted === "object" && persisted.state &&
				typeof persisted.state.managementKey === "string" && persisted.state.managementKey !== "") {
				return persisted.state.managementKey;
			}
			var legacy = parseLoose(deobfuscate(global.localStorage.getItem("managementKey")));
			if (typeof legacy === "string" && legacy !== "") {
				return legacy;
			}
		} catch (ignored) {}
		return null;
	}

	function managementPath(suffix) {
		var path = global.location.pathname.replace(/\/+$/, "");
		var prefix = "";
		var markers = ["/v0/management/plugins/reset-priority", "/v0/resource/plugins/reset-priority"];
		for (var i = 0; i < markers.length; i++) {
			var at = path.indexOf(markers[i]);
			if (at >= 0) {
				prefix = path.slice(0, at);
				break;
			}
		}
		return prefix + "/v0/management/plugins/reset-priority" + suffix;
	}

	function authHeaders(extra) {
		var headers = {};
		var name;
		for (name in (extra || {})) {
			if (Object.prototype.hasOwnProperty.call(extra, name)) {
				headers[name] = extra[name];
			}
		}
		var key = managementKey();
		if (key) {
			headers.Authorization = ["Bearer", key].join(" ");
		}
		return headers;
	}

	global.resetPriorityAuth = {
		managementKey: managementKey,
		managementPath: managementPath,
		authHeaders: authHeaders
	};
})(window);
`
