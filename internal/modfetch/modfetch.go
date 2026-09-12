// Package modfetch obtains module versions from outside the repository: a
// Source decides the mechanism, the go command today, callers see versions
// and trees.
//
// It is the one place go-whatchanged runs a command, reaches the network or
// writes to disk, and a run without a Source does none of the three: that
// is the whole of --fsreadonly. Anything new that must do any of them
// belongs behind Source, so that leaving the Source out keeps leaving it
// out; what a Source-less run cannot do fails with an error wrapping
// ErrReadOnly.
package modfetch

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"golang.org/x/mod/module"

	"github.com/shazow/go-whatchanged/internal/vfs"
)

// Source obtains module versions. A nil Source keeps the tool in its
// read-only mode: a module missing from the module cache is an error rather
// than a download, and a module version cannot be diffed.
type Source interface {
	// Resolve turns a query into the version it denotes: a semantic
	// version, "latest", a branch or tag name, a commit prefix, or a
	// comparison such as "<v1.5.0". A canonical version of the path's own
	// major version resolves to itself, so a Source that cannot evaluate
	// queries may still serve exact ones. Resolve may answer under a
	// different path than it was asked for, and its error may name one:
	// see DeclaredPath.
	Resolve(ctx context.Context, path, query string) (module.Version, error)

	// Fetch obtains one module version and says where its tree is readable.
	// It is idempotent and safe for concurrent use: the two sides of a diff
	// load in parallel and may ask for the same dependency at once.
	Fetch(ctx context.Context, mod module.Version) (*Module, error)

	// Prefetch obtains mods as a batch, ahead of need, so that Fetch then
	// answers from it. A go.mod at go 1.17 or later names every module its
	// packages and tests can import, so fetching its missing requirements
	// together, before type-checking starts, replaces one download per
	// import with one parallel download, at the price of the modules only
	// tests need. A problem with one module is not an error here: it
	// surfaces from Fetch, when and if that module is needed. The error is
	// for the batch as a whole and advisory, since Fetch reports it again.
	// A Source with nothing to gain from batching may do nothing.
	Prefetch(ctx context.Context, mods []module.Version) error
}

// ErrReadOnly is what a run without a Source cannot do: resolve or fetch a
// module version, or download a module the cache lacks. The errors of such
// a run wrap it and say what was needed; the command line recognizes it and
// names the flag to drop, which no error message here does.
var ErrReadOnly = errors.New("the go command is off limits in a read-only run")

// Module is a fetched module version.
type Module struct {
	module.Version

	// Dir is the absolute path of the module root. With FS nil it is a
	// directory on the host. Otherwise it is the synthetic path FS must be
	// mounted at, and FS serves the module root at ".".
	Dir string
	FS  vfs.FS

	// GoMod is the module's go.mod as the Source knows it: the file from
	// the tree, or the one the ecosystem synthesizes for versions that
	// predate modules. Resolution reads this rather than Dir, so that a
	// module without a go.mod of its own still resolves.
	GoMod []byte
}

// declaredPath matches the module path a go command error reports as the
// one the module's go.mod declares. The go command refuses the mismatch
// rather than following it, so its message is where the declared path is
// to be had: "go.mod has post-v2 module path "charm.land/lipgloss/v2" at
// revision v2.0.0" for a vanity path or a missing major version suffix,
// and "module declares its path as: X" for a proxied .mod file.
var declaredPath = []*regexp.Regexp{
	regexp.MustCompile(`module path "([^"]+)" at revision`),
	regexp.MustCompile(`module path must match major version \("([^"]+)"\)`),
	regexp.MustCompile(`module declares its path as:\s*(\S+)`),
}

// DeclaredPath returns the module path err says the module declares for
// itself, or "" when err is not a module path mismatch or names nothing
// usable. The caller decides whether to follow it: only a side of the diff
// may be, since a dependency's path is the import path its importers
// spell, which nothing is free to redirect.
func DeclaredPath(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, re := range declaredPath {
		m := re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		// A malformed path is reported the same way; it is no redirect.
		if p := strings.TrimSpace(m[1]); module.CheckPath(p) == nil {
			return p
		}
	}
	return ""
}
