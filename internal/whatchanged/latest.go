package whatchanged

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/shazow/go-whatchanged/internal/modfetch"
	"github.com/shazow/go-whatchanged/internal/modres"
	"github.com/shazow/go-whatchanged/internal/release"
)

// LatestRelease is the pseudo-revision that names the newest release tag of
// the module among the ancestors of the head commit: "what changed since the
// last release". Release tags are the ones the go command would publish,
// "v1.2.3" (or "sub/v1.2.3" for a module in a subdirectory) with the major
// version the module path calls for. As the base, tags on the head commit
// itself do not count, so that on a freshly tagged commit the diff describes
// that release instead of being empty; as the head, where the point is to
// name the release rather than to skip it, they do.
const LatestRelease = "@latest"

// PreviousRelease is the pseudo-revision that names the release tag before
// the newest one reachable from the head, the newest counting a tag on the
// head commit itself: "the release before the last one". It is only a base,
// and with no head of its own the head becomes LatestRelease, so that
// @previous alone compares the two newest releases: what the last release
// shipped.
const PreviousRelease = "@previous"

// previousQuery is how PreviousRelease is spelled for a module side, where
// "latest" likewise comes without the @. For a module it is the version
// below the one "latest" resolves to.
const previousQuery = "previous"

// isReleaseQuery reports whether rev is one of the pseudo-revisions that
// name a release tag rather than a revision git knows.
func isReleaseQuery(rev string) bool {
	return rev == LatestRelease || rev == PreviousRelease
}

// resolveSides turns the sides' queries into concrete revisions and
// versions: a module side's query through Options.Fetch, and a git side's
// LatestRelease or PreviousRelease into a release tag. It also reports the
// semantic version the base denotes, when it is a release tag or a released
// module version, so that the summary can suggest the next version.
func resolveSides(ctx context.Context, base, head sideSpec, env modres.Env, opts Options) (b, h sideSpec, baseVersion string, err error) {
	asked := base.mod.Path
	if base.mod.Path != "" {
		if opts.Fetch == nil {
			return base, head, "", readOnlyError(base.mod)
		}
		if base.mod, err = resolveModule(ctx, base.mod, opts); err != nil {
			return base, head, "", err
		}
		if v := base.mod.Version; !module.IsPseudoVersion(v) && semver.IsValid(v) {
			baseVersion = v
		}
	}
	if head.mod.Path != "" {
		if opts.Fetch == nil {
			return base, head, "", readOnlyError(head.mod)
		}
		resolved := false
		// A head that named the base's location is the base's module, so
		// it follows the base to whatever path that location turned out to
		// have. Left behind, a query like @HEAD or @main would resolve
		// under the old path, which for a module that has moved on is the
		// major version before this one: two module lines diffed against
		// each other. A version the base's path has nothing to offer, as
		// in a diff across the move, stays where it was named.
		if head.mod.Path == asked && base.mod.Path != asked {
			if v, err := opts.Fetch.Resolve(ctx, base.mod.Path, head.mod.Version); err == nil {
				head.mod, resolved = v, true
			}
		}
		if !resolved {
			if head.mod, err = resolveModule(ctx, head.mod, opts); err != nil {
				return base, head, "", err
			}
		}
	}
	// The head is resolved first: the base's own release query is named
	// relative to the head, so the head cannot still be a query itself.
	if head.rev == LatestRelease {
		// A head the base's @previous supplied is part of resolving
		// @previous, so its errors are reported under that name.
		query := LatestRelease
		if base.rev == PreviousRelease {
			query = PreviousRelease
		}
		if head.rev, err = resolveHeadRelease(ctx, head, env, opts, query); err != nil {
			return base, head, "", err
		}
	}
	if base.mod.Path != "" {
		return base, head, baseVersion, nil
	}
	rev, version, err := resolveBase(ctx, base, head, env, opts)
	if err != nil {
		return base, head, "", err
	}
	base.rev = rev
	return base, head, version, nil
}

// resolveModule turns a module side's query into the version it denotes.
// Every query but previousQuery goes straight to the Source; that one is
// the highest version below the one "latest" resolves to.
func resolveModule(ctx context.Context, mod module.Version, opts Options) (module.Version, error) {
	if mod.Version != previousQuery {
		return resolveQuery(ctx, mod.Path, mod.Version, opts)
	}
	latest, err := resolveQuery(ctx, mod.Path, "latest", opts)
	if err != nil {
		return module.Version{}, err
	}
	// The path "latest" answered under is the module's own, so the search
	// for the version below it needs no second redirect.
	prev, err := opts.Fetch.Resolve(ctx, latest.Path, "<"+latest.Version)
	if err != nil {
		return module.Version{}, fmt.Errorf("%s: no release of %s before %s: %w", PreviousRelease, latest.Path, latest.Version, err)
	}
	return prev, nil
}

// resolveQuery resolves one query, following the module path the go
// command reports when the location the side names is not the path the
// module declares for itself; see redirect.
func resolveQuery(ctx context.Context, path, query string, opts Options) (module.Version, error) {
	v, err := opts.Fetch.Resolve(ctx, path, query)
	if err == nil {
		return v, nil
	}
	declared, ok := redirect(path, err, opts)
	if !ok {
		return module.Version{}, err
	}
	alt, altErr := opts.Fetch.Resolve(ctx, declared, query)
	if altErr != nil {
		// The module the location named is what was asked for, so its
		// error is the one to report.
		return module.Version{}, err
	}
	return alt, nil
}

// redirect reports the module path to retry path under, from the error the
// go command gave for it: a module whose go.mod declares a vanity path, or
// one whose major version suffix the location lacks
// (github.com/x/m@v2.0.0, whose module path is github.com/x/m/v2). The go
// command refuses such a pair outright; go-whatchanged only reads the
// module, so it can follow the path instead, which is the module the
// location meant. Only a side of the diff is ever redirected, and only one
// hop, to the path the module itself declares.
func redirect(path string, err error, opts Options) (string, bool) {
	if opts.ExactModulePath {
		return "", false
	}
	declared := modfetch.DeclaredPath(err)
	return declared, declared != "" && declared != path
}

// resolveHeadRelease turns a head of LatestRelease into the newest release
// tag of its repository, named from HEAD and counting a tag on HEAD itself:
// the last release, whether or not it is the commit checked out. query is
// the pseudo-revision its errors are reported under.
func resolveHeadRelease(ctx context.Context, head sideSpec, env modres.Env, opts Options, query string) (string, error) {
	// The module path comes from HEAD: the head side is the tag this is
	// about to find, so it cannot be read yet.
	probe := head
	probe.rev = "HEAD"
	modPath, err := headModulePath(ctx, probe, env, opts)
	if err != nil {
		return "", err
	}
	tags, err := release.TagsFor(modPath, head.rel)
	if err != nil {
		return "", fmt.Errorf("%s: %w", query, err)
	}
	repo, err := head.open()
	if err != nil {
		return "", err
	}
	hash, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return "", err
	}
	found, err := reachableTags(repo, tags, hash, "HEAD", modPath)
	if err == nil {
		var c candidate
		if c, err = pickRelease(pickHead, found, "HEAD"); err == nil {
			return c.tag, nil
		}
	}
	return "", fmt.Errorf("%s: %w", query, err)
}

// resolveBase turns a git base revision into a concrete one, resolving a
// release query against the head's module, and reports the semantic version
// the base denotes when it is a release tag of the module. Problems reading
// the head side's go.mod are left for loadSide to report, unless the release
// query depends on it.
func resolveBase(ctx context.Context, base, head sideSpec, env modres.Env, opts Options) (rev, version string, err error) {
	var tags release.Tags
	modPath := head.mod.Path
	if modPath == "" {
		modPath, err = headModulePath(ctx, head, env, opts)
	}
	if err == nil {
		tags, err = release.TagsFor(modPath, base.rel)
	}
	if !isReleaseQuery(base.rev) {
		if err != nil {
			return base.rev, "", nil
		}
		return base.rev, tags.Version(tagName(base.rev)), nil
	}
	// Problems with the head side name it themselves; only the search for
	// the tag is reported as the query's.
	if err != nil {
		return "", "", err
	}
	repo, err := base.open()
	if err != nil {
		return "", "", err
	}
	headRev := head.rev
	if headRev == "" {
		headRev = "HEAD"
	}
	headHash, err := resolveCommit(repo, headRev)
	if err != nil {
		return "", "", err
	}
	which := pickLatest
	if base.rev == PreviousRelease {
		which = pickPrevious
	}
	found, err := reachableTags(repo, tags, headHash, headRev, modPath)
	if err == nil {
		var c candidate
		if c, err = pickRelease(which, found, headRev); err == nil {
			return c.tag, c.version, nil
		}
	}
	return "", "", fmt.Errorf("%s: %w", base.rev, err)
}

// tagName strips the ref prefixes a user may type in front of a tag.
func tagName(rev string) string {
	rev = strings.TrimPrefix(rev, "refs/")
	return strings.TrimPrefix(rev, "tags/")
}

// headModulePath reads the module path from the head side's go.mod.
func headModulePath(ctx context.Context, head sideSpec, env modres.Env, opts Options) (string, error) {
	s, err := head.mount(ctx, opts)
	if err != nil {
		return "", err
	}
	res, err := s.resolver(env)
	if err != nil {
		return "", err
	}
	return res.ModPath(), nil
}

// candidate is a release tag reachable from the head, the version it
// denotes, and whether it sits on the head commit itself.
type candidate struct {
	tag, version string
	onHead       bool
}

// pick names which of the tags reachable from the head a release query
// wants.
type pick int

const (
	// pickLatest is the base's LatestRelease: the newest release tag that
	// is not on the head commit.
	pickLatest pick = iota
	// pickPrevious is PreviousRelease: the one below the newest, the head
	// commit's own tag counting as the newest.
	pickPrevious
	// pickHead is the head's LatestRelease: the newest release tag, the
	// head commit's own included.
	pickHead
)

// pickRelease chooses among the release tags reachable from the head,
// ranked newest version first. Its errors describe what the repository has
// instead of what was asked for.
func pickRelease(which pick, found []candidate, headRev string) (candidate, error) {
	switch which {
	case pickPrevious:
		if len(found) > 1 {
			return found[1], nil
		}
		return candidate{}, fmt.Errorf("%s is the only release tag reachable from %s, and %s is the one before the newest; name a base instead: @%s",
			found[0].tag, headRev, PreviousRelease, found[0].tag)
	case pickHead:
		return found[0], nil
	default:
		if i := slices.IndexFunc(found, func(c candidate) bool { return !c.onHead }); i >= 0 {
			return found[i], nil
		}
		// A freshly tagged release, whose diff against itself would be
		// empty: every reachable tag is on the head commit.
		return candidate{}, fmt.Errorf("%s is the newest release tag reachable from %s, but it is on %s itself, which %s skips; name it instead: @%s",
			found[0].tag, headRev, headRev, LatestRelease, found[0].tag)
	}
}

// reachableTags returns the release tags of modPath that are reachable from
// head, ranked by version, newest first, and never empty: a repository with
// none to offer is an error that says why. Annotated tags are followed to
// the commit they point at; tags on anything but a commit are ignored.
func reachableTags(repo *git.Repository, tags release.Tags, head plumbing.Hash, headRev, modPath string) ([]candidate, error) {
	byCommit := map[plumbing.Hash][]candidate{}
	refs, err := repo.Tags()
	if err != nil {
		return nil, err
	}
	total := 0
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		v := tags.Version(name)
		if v == "" {
			return nil
		}
		h := ref.Hash()
		if t, err := repo.TagObject(h); err == nil {
			c, err := t.Commit()
			if err != nil {
				return nil
			}
			h = c.Hash
		}
		total++
		byCommit[h] = append(byCommit[h], candidate{tag: name, version: v})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if total == 0 {
		switch {
		case shallowHint(repo) != "":
			return nil, fmt.Errorf("no release tags for %s (looking for tags like %q)%s", modPath, tags.Example(), shallowHint(repo))
		case !hasTags(repo):
			return nil, fmt.Errorf("no release tags for %s (looking for tags like %q); the clone has no tags at all: git fetch --tags brings them, if the repository has any", modPath, tags.Example())
		default:
			// The tags there are do not fit the module: a nested module
			// tagged without its directory prefix, or the other way around.
			return nil, fmt.Errorf("no release tags for %s (looking for tags like %q; tags: %s)", modPath, tags.Example(), fewOf(tagNames(repo)))
		}
	}

	// Walk head and its ancestors, stopping once every tagged commit has
	// been seen.
	var found []candidate
	remaining := len(byCommit)
	log, err := repo.Log(&git.LogOptions{From: head})
	if err != nil {
		return nil, err
	}
	err = log.ForEach(func(c *object.Commit) error {
		cands, ok := byCommit[c.Hash]
		if !ok {
			return nil
		}
		for _, cand := range cands {
			cand.onHead = c.Hash == head
			found = append(found, cand)
		}
		remaining--
		if remaining == 0 {
			return storer.ErrStop
		}
		return nil
	})
	// A shallow clone ends without the parents of its oldest commits:
	// the history reachable from head simply stops there.
	if err != nil && !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, err
	}
	if len(found) == 0 {
		var names []string
		for _, cands := range byCommit {
			for _, c := range cands {
				names = append(names, c.tag)
			}
		}
		slices.SortFunc(names, func(a, b string) int { return semver.Compare(tags.Version(b), tags.Version(a)) })
		return nil, fmt.Errorf("none of the %d release tag(s) for %s (%s) is an ancestor of %s%s", total, modPath, fewOf(names), headRev, shallowHint(repo))
	}
	slices.SortFunc(found, func(a, b candidate) int { return semver.Compare(b.version, a.version) })
	return found, nil
}
