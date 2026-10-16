package main

// Telling the writer when a newer Lotus exists.

import (
	"github.com/MohammadYzbk/lotus/internal/update"
)

// version is the running build, stamped in at link time:
//
//	wails build -ldflags "-X main.version=1.2.0"
//
// An unstamped build is a development build, and is never told it is out of
// date — it is not on the release line at all.
var version = update.DevVersion

// updateRepo is where releases are published. A variable rather than a
// constant so a fork can point it elsewhere with -X main.updateRepo=…
var updateRepo = "MohammadYzbk/lotus"

// AppVersion reports the running build, for the About line.
func (a *App) AppVersion() string { return version }

// CheckForUpdate asks GitHub whether there is a newer release.
//
// Never automatic. An offline-first editor that phones home on launch is not
// offline-first, and the one thing a writer on a metered connection does not
// want is the app deciding to use it.
func (a *App) CheckForUpdate() update.Result {
	return update.Checker{Repo: updateRepo}.Check(a.context(), version)
}
