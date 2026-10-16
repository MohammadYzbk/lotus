package engine

import (
	"fmt"
	"runtime"
)

// Version is the Tectonic release this build of Lotus installs and bundles.
//
// Pinned rather than tracking "latest": a document that compiled yesterday
// should compile today, and an engine that changes under the writer without
// anyone choosing it is how that stops being true. Moving it is a deliberate
// edit, with the digests below re-derived at the same time.
const Version = "0.17.0"

// releaseTag is the Git tag Tectonic publishes under, already escaped for a
// URL path. The tag really does contain an "@"; it is written %40 here rather
// than run through url.PathEscape, which leaves "@" alone because it is legal
// in a path segment — and %40 is the form GitHub publishes.
const releaseTag = "tectonic%40" + Version

// Archive is how an asset is packed.
type Archive string

const (
	ArchiveTarGz Archive = "tar.gz"
	ArchiveZip   Archive = "zip"
)

// Asset is one platform's download.
type Asset struct {
	// Name is the file as published.
	Name string
	// SHA256 is the expected digest, lowercase hex.
	SHA256 string
	// Archive is how to unpack it.
	Archive Archive

	// urlOverride redirects the download, for tests. Unexported so that no
	// caller outside this package can point an install somewhere else.
	urlOverride string
}

// URL is where the asset is downloaded from.
func (a Asset) URL() string {
	if a.urlOverride != "" {
		return a.urlOverride
	}
	return "https://github.com/tectonic-typesetting/tectonic/releases/download/" +
		releaseTag + "/" + a.Name
}

// assets maps GOOS/GOARCH to the release asset for it.
//
// Linux takes the musl builds. They are statically linked, so they run on a
// distribution whose glibc is older than the build machine's — which the gnu
// builds do not, and that failure appears as an unreadable loader error at the
// writer's end rather than at ours.
//
// The digests are GitHub's own for these assets. They are not an independent
// attestation — the same host serves both — but they are what catches a
// truncated or interrupted download, and they pin the bytes so that replacing
// the file behind the URL no longer goes unnoticed.
var assets = map[string]Asset{
	"darwin/arm64": {
		Name:    "tectonic-" + Version + "-aarch64-apple-darwin.tar.gz",
		SHA256:  "a3f1cac7c5678f01661a92212f58480ae3b0634115d880dbc59e2953ded45667",
		Archive: ArchiveTarGz,
	},
	"darwin/amd64": {
		Name:    "tectonic-" + Version + "-x86_64-apple-darwin.tar.gz",
		SHA256:  "7c90ef5b6ddb1eb1937e4337add5237b79338e4b9676459fa91187d24d6cdf80",
		Archive: ArchiveTarGz,
	},
	"linux/amd64": {
		Name:    "tectonic-" + Version + "-x86_64-unknown-linux-musl.tar.gz",
		SHA256:  "8533d07f9ccbd7a65824b9e0459041bca34af1eb33daba48f59215593753a3b7",
		Archive: ArchiveTarGz,
	},
	"linux/arm64": {
		Name:    "tectonic-" + Version + "-aarch64-unknown-linux-musl.tar.gz",
		SHA256:  "b10954a95404f3ab2328d2fa59a5ebab8e657f893fab096f98be8db7c0c979b8",
		Archive: ArchiveTarGz,
	},
	"windows/amd64": {
		Name:    "tectonic-" + Version + "-x86_64-pc-windows-msvc.zip",
		SHA256:  "f61ce51f0b0ade1015b7de7ef368541c5424e9756ecbd0d7af97d6d48030845f",
		Archive: ArchiveZip,
	},
}

// ErrUnsupportedPlatform means Tectonic publishes no build we can install here.
type ErrUnsupportedPlatform struct {
	GOOS, GOARCH string
}

func (e ErrUnsupportedPlatform) Error() string {
	return fmt.Sprintf("no Tectonic %s build for %s/%s — install one yourself and put it on PATH",
		Version, e.GOOS, e.GOARCH)
}

// AssetFor returns the download for a platform.
func AssetFor(goos, goarch string) (Asset, error) {
	a, ok := assets[goos+"/"+goarch]
	if !ok {
		return Asset{}, ErrUnsupportedPlatform{GOOS: goos, GOARCH: goarch}
	}
	return a, nil
}

// SupportedPlatforms lists every GOOS/GOARCH an engine can be installed for.
func SupportedPlatforms() []string {
	out := make([]string, 0, len(assets))
	for k := range assets {
		out = append(out, k)
	}
	return out
}

// HostAsset returns the download for the running platform.
func HostAsset() (Asset, error) { return AssetFor(runtime.GOOS, runtime.GOARCH) }
