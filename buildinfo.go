package main

// These values are injected at compile time with:
//   wails build -ldflags "-X main.buildCode=<code> -X main.appVersion=<ver>"
// See build-group.ps1. Their zero values give a sensible "developer build"
// behaviour: no remote fetch, use the local/embedded config.json.
var (
	// buildCode identifies this build's user/group to the remote endpoint.
	// Empty => no remote config (use exe-adjacent or embedded config.json).
	buildCode = ""

	// appVersion is surfaced in logs and Discord embeds.
	appVersion = "dev"

	// remoteConfigURL returns this build's per-user config when given ?code=.
	// Served by the launcher Worker on its own subdomain (a Worker Custom Domain),
	// so it never clashes with the main site on deforce.site. Overridable via
	// -ldflags to point at a staging Worker.
	remoteConfigURL = "https://api.deforce.site/"

	// requireUserCode = "true" marks a UNIVERSAL build: it has no baked-in code,
	// so on first launch it prompts the user for their personal code (and then
	// remembers it). Default "false" = developer build using the embedded config.
	requireUserCode = "false"

	// githubRepo (owner/name) is used to show release notes after a self-update.
	githubRepo = "deforcy/ChromaCube_Launcher"
)

// codeMode reports whether this build identifies users by a typed-in code (a
// universal build with no baked-in buildCode).
func codeMode() bool {
	return buildCode == "" && requireUserCode == "true"
}
