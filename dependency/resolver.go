package dependency

import (
	"container/list"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/glide/cfg"
	"github.com/Masterminds/glide/msg"
	"github.com/Masterminds/glide/util"
)

// MissingPackageHandler handles the case where a package is missing during scanning.
//
// It returns true if the package can be passed to the resolver, false otherwise.
// False may be returned even if error is nil.
type MissingPackageHandler interface {
	// NotFound is called when the Resolver fails to find a package with the given name.
	//
	// NotFound returns true when the resolver should attempt to re-resole the
	// dependency (e.g. when NotFound has gone and fetched the missing package).
	//
	// When NotFound returns false, the Resolver does not try to do any additional
	// work on the missing package.
	//
	// NotFound only returns errors when it fails to perform its internal goals.
	// When it returns false with no error, this indicates that the handler did
	// its job, but the resolver should not do any additional work on the
	// package.
	NotFound(pkg string) (bool, error)

	// OnGopath is called when the Resolver finds a dependency, but it's only on GOPATH.
	//
	// OnGopath provides an opportunity to copy, move, warn, or ignore cases like this.
	//
	// OnGopath returns true when the resolver should attempt to re-resolve the
	// dependency (e.g. when the dependency is copied to a new location).
	//
	// When OnGopath returns false, the Resolver does not try to do any additional
	// work on the package.
	//
	// An error indicates that OnGopath cannot complete its intended operation.
	// Not all false results are errors.
	OnGopath(pkg string) (bool, error)
}

// DefaultMissingPackageHandler is the default handler for missing packages.
//
// When asked to handle a missing package, it will report the miss as a warning,
// and then store the package in the Missing slice for later access.
type DefaultMissingPackageHandler struct {
	Missing []string
	Gopath  []string
}

// NotFound prints a warning and then stores the package name in Missing.
//
// It never returns an error, and it always returns false.
func (d *DefaultMissingPackageHandler) NotFound(pkg string) (bool, error) {
	msg.Warn("Package %s is not installed", pkg)
	d.Missing = append(d.Missing, pkg)
	return false, nil
}

func (d *DefaultMissingPackageHandler) OnGopath(pkg string) (bool, error) {
	msg.Warn("Package %s is only on GOPATH.", pkg)
	d.Gopath = append(d.Gopath, pkg)
	return false, nil
}

// Resolver resolves a dependency tree.
//
// It operates in two modes:
// - local resolution (ResolveLocal) determines the dependencies of the local project.
// - vendor resolving (Resolve, ResolveAll) determines the dependencies of vendored
//   projects.
//
// Local resolution is for guessing initial dependencies. Vendor resolution is
// for determining vendored dependencies.
type Resolver struct {
	Handler      MissingPackageHandler
	basedir      string
	VendorDir    string
	BuildContext *util.BuildCtxt
	seen         map[string]bool

	// Items already in the queue.
	alreadyQ map[string]bool

	// findCache caches hits from Find. This reduces the number of filesystem
	// touches that have to be done for dependency resolution.
	findCache map[string]*PkgInfo
}

// NewResolver returns a new Resolver initialized with the DefaultMissingPackageHandler.
//
// This will return an error if the given path does not meet the basic criteria
// for a Go source project. For example, basedir must have a vendor subdirectory.
//
// The BuildContext uses the "go/build".Default to resolve dependencies.
func NewResolver(basedir string) (*Resolver, error) {

	var err error
	basedir, err = filepath.Abs(basedir)
	if err != nil {
		return nil, err
	}
	vdir := filepath.Join(basedir, "vendor")

	buildContext, err := util.GetBuildContext()
	if err != nil {
		return nil, err
	}

	r := &Resolver{
		Handler:      &DefaultMissingPackageHandler{Missing: []string{}, Gopath: []string{}},
		basedir:      basedir,
		VendorDir:    vdir,
		BuildContext: buildContext,
		seen:         map[string]bool{},
		alreadyQ:     map[string]bool{},
		findCache:    map[string]*PkgInfo{},
	}

	// TODO: Make sure the build context is correctly set up. Especially in
	// regards to GOROOT, which is not always set.

	return r, nil
}

// Resolve takes a package name and returns all of the imported package names.
//
// If a package is not found, this calls the Fetcher. If the Fetcher returns
// true, it will re-try traversing that package for dependencies. Otherwise it
// will add that package to the deps array and continue on without trying it.
// And if the Fetcher returns an error, this will stop resolution and return
// the error.
//
// If basepath is set to $GOPATH, this will start from that package's root there.
// If basepath is set to a project's vendor path, the scanning will begin from
// there.
func (r *Resolver) Resolve(pkg, basepath string) ([]string, error) {
	target := filepath.Join(basepath, filepath.FromSlash(pkg))
	//msg.Debug("Scanning %s", target)
	l := list.New()
	l.PushBack(target)
	return r.resolveList(l)
}

// ResolveLocal resolves dependencies for the current project.
//
// This begins with the project, builds up a list of external dependencies.
//
// If the deep flag is set to true, this will then resolve all of the dependencies
// of the dependencies it has found. If not, it will return just the packages that
// the base project relies upon.
func (r *Resolver) ResolveLocal(deep bool) ([]string, error) {
	// We build a list of local source to walk, then send this list
	// to resolveList.
	l := list.New()
	alreadySeen := map[string]bool{}
	err := filepath.Walk(r.basedir, func(path string, fi os.FileInfo, err error) error {
		if err != nil && err != filepath.SkipDir {
			return err
		}
		if !fi.IsDir() {
			return nil
		}
		if !srcDir(fi) {
			return filepath.SkipDir
		}

		// Scan for dependencies, and anything that's not part of the local
		// package gets added to the scan list.
		p, err := r.BuildContext.ImportDir(path, 0)
		if err != nil {
			if strings.HasPrefix(err.Error(), "no buildable Go source") {
				return nil
			}
			return err
		}

		// We are only looking for dependencies in vendor. No root, cgo, etc.
		for _, imp := range p.Imports {
			if alreadySeen[imp] {
				continue
			}
			alreadySeen[imp] = true
			info := r.FindPkg(imp)
			switch info.Loc {
			case LocUnknown, LocVendor:
				l.PushBack(filepath.Join(r.VendorDir, filepath.FromSlash(imp))) // Do we need a path on this?
			case LocGopath:
				if !strings.HasPrefix(info.Path, r.basedir) {
					// FIXME: This is a package outside of the project we're
					// scanning. It should really be on vendor. But we don't
					// want it to reference GOPATH. We want it to be detected
					// and moved.
					l.PushBack(filepath.Join(r.VendorDir, filepath.FromSlash(imp)))
				}
			}
		}

		return nil
	})

	if err != nil {
		msg.Error("Failed to build an initial list of packages to scan: %s", err)
		return []string{}, err
	}

	if deep {
		return r.resolveList(l)
	}

	// If we're not doing a deep scan, we just convert the list into an
	// array and return.
	res := make([]string, 0, l.Len())
	for e := l.Front(); e != nil; e = e.Next() {
		res = append(res, e.Value.(string))
	}
	return res, nil
}

// ResolveAll takes a list of packages and returns an inclusive list of all
// vendored dependencies.
//
// While this will scan all of the source code it can find, it will only return
// packages that were either explicitly passed in as deps, or were explicitly
// imported by the code.
//
// Packages that are either CGO or on GOROOT are ignored. Packages that are
// on GOPATH, but not vendored currently generate a warning.
//
// If one of the passed in packages does not exist in the vendor directory,
// an error is returned.
func (r *Resolver) ResolveAll(deps []*cfg.Dependency) ([]string, error) {
	queue := sliceToQueue(deps, r.VendorDir)
	return r.resolveList(queue)
}

// resolveList takes a list and resolves it.
func (r *Resolver) resolveList(queue *list.List) ([]string, error) {

	var failedDep string
	for e := queue.Front(); e != nil; e = e.Next() {
		dep := e.Value.(string)
		//msg.Warn("#### %s ####", dep)
		//msg.Info("Seen Count: %d", len(r.seen))
		// Catch the outtermost dependency.
		failedDep = dep
		err := filepath.Walk(dep, func(path string, fi os.FileInfo, err error) error {
			if err != nil && err != filepath.SkipDir {
				return err
			}

			// Skip files.
			if !fi.IsDir() {
				return nil
			}
			// Skip dirs that are not source.
			if !srcDir(fi) {
				//msg.Debug("Skip resource %s", fi.Name())
				return filepath.SkipDir
			}

			// Anything that comes through here has already been through
			// the queue.
			r.alreadyQ[path] = true
			e := r.queueUnseen(path, queue)
			if err != nil {
				failedDep = path
				//msg.Error("Failed to fetch dependency %s: %s", path, err)
			}
			return e
		})
		if err != nil && err != filepath.SkipDir {
			msg.Error("Dependency %s failed to resolve: %s.", failedDep, err)
			return []string{}, err
		}
	}

	res := make([]string, 0, queue.Len())
	for e := queue.Front(); e != nil; e = e.Next() {
		res = append(res, e.Value.(string))
	}

	return res, nil
}

// queueUnseenImports scans a package's imports and adds any new ones to the
// processing queue.
func (r *Resolver) queueUnseen(pkg string, queue *list.List) error {
	// A pkg is marked "seen" as soon as we have inspected it the first time.
	// Seen means that we have added all of its imports to the list.

	// Already queued indicates that we've either already put it into the queue
	// or intentionally not put it in the queue for fatal reasons (e.g. no
	// buildable source).

	deps, err := r.imports(pkg)
	if err != nil && !strings.HasPrefix(err.Error(), "no buildable Go source") {
		msg.Error("Could not find %s: %s", pkg, err)
		return err
		// NOTE: If we uncomment this, we get lots of "no buildable Go source" errors,
		// which don't ever seem to be helpful. They don't actually indicate an error
		// condition, and it's perfectly okay to run into that condition.
		//} else if err != nil {
		//	msg.Warn(err.Error())
	}

	for _, d := range deps {
		if _, ok := r.alreadyQ[d]; !ok {
			r.alreadyQ[d] = true
			queue.PushBack(d)
		}
	}
	return nil
}

// imports gets all of the imports for a given package.
//
// If the package is in GOROOT, this will return an empty list (but not
// an error).
// If it cannot resolve the pkg, it will return an error.
func (r *Resolver) imports(pkg string) ([]string, error) {

	// If this pkg is marked seen, we don't scan it again.
	if _, ok := r.seen[pkg]; ok {
		msg.Debug("Already saw %s", pkg)
		return []string{}, nil
	}

	// FIXME: On error this should try to NotFound to the dependency, and then import
	// it again.
	p, err := r.BuildContext.ImportDir(pkg, 0)
	if err != nil {
		return []string{}, err
	}

	// It is okay to scan a package more than once. In some cases, this is
	// desirable because the package can change between scans (e.g. as a result
	// of a failed scan resolving the situation).
	msg.Debug("=> Scanning %s (%s)", p.ImportPath, pkg)
	r.seen[pkg] = true

	// Optimization: If it's in GOROOT, it has no imports worth scanning.
	if p.Goroot {
		return []string{}, nil
	}

	// We are only looking for dependencies in vendor. No root, cgo, etc.
	buf := []string{}
	for _, imp := range p.Imports {
		info := r.FindPkg(imp)
		switch info.Loc {
		case LocUnknown:
			// Do we resolve here?
			found, err := r.Handler.NotFound(imp)
			if err != nil {
				msg.Error("Failed to fetch %s: %s", imp, err)
			}
			if found {
				buf = append(buf, filepath.Join(r.VendorDir, filepath.FromSlash(imp)))
				continue
			}
			r.seen[info.Path] = true
		case LocVendor:
			//msg.Debug("Vendored: %s", imp)
			buf = append(buf, info.Path)
		case LocGopath:
			found, err := r.Handler.OnGopath(imp)
			if err != nil {
				msg.Error("Failed to fetch %s: %s", imp, err)
			}
			// If the Handler marks this as found, we drop it into the buffer
			// for subsequent processing. Otherwise, we assume that we're
			// in a less-than-perfect, but functional, situation.
			if found {
				buf = append(buf, filepath.Join(r.VendorDir, filepath.FromSlash(imp)))
				continue
			}
			msg.Warn("Package %s is on GOPATH, but not vendored. Ignoring.", imp)
			r.seen[info.Path] = true
		default:
			// Local packages are an odd case. CGO cannot be scanned.
			msg.Debug("===> Skipping %s", imp)
		}
	}

	return buf, nil
}

// sliceToQueue is a special-purpose function for unwrapping a slice of
// dependencies into a queue of fully qualified paths.
func sliceToQueue(deps []*cfg.Dependency, basepath string) *list.List {
	l := list.New()
	for _, e := range deps {
		l.PushBack(filepath.Join(basepath, filepath.FromSlash(e.Name)))
	}
	return l
}

// PkgLoc describes the location of the package.
type PkgLoc uint8

const (
	// LocUnknown indicates the package location is unknown (probably not present)
	LocUnknown PkgLoc = iota
	// LocLocal inidcates that the package is in a local dir, not GOPATH or GOROOT.
	LocLocal
	// LocVendor indicates that the package is in a vendor/ dir
	LocVendor
	// LocGopath inidcates that the package is in GOPATH
	LocGopath
	// LocGoroot indicates that the package is in GOROOT
	LocGoroot
	// LocCgo indicates that the package is a a CGO package
	LocCgo
)

type PkgInfo struct {
	Name, Path string
	Vendored   bool
	Loc        PkgLoc
}

// FindPkg takes a package name and attempts to find it on the filesystem
//
// The resulting PkgInfo will indicate where it was found.
func (r *Resolver) FindPkg(name string) *PkgInfo {
	// We cachae results for FindPkg to reduce the number of filesystem ops
	// that we have to do. This is a little risky because certain directories,
	// like GOPATH, can be modified while we're running an operation, and
	// render the cache inaccurate.
	//
	// Unfound items (LocUnkown) are never cached because we assume that as
	// part of the response, the Resolver may fetch that dependency.
	if i, ok := r.findCache[name]; ok {
		//msg.Info("Cache hit on %s", name)
		return i
	}

	// 502 individual packages scanned.
	// No cache:
	// glide -y etcd.yaml list  0.27s user 0.19s system 85% cpu 0.534 total
	// With cache:
	// glide -y etcd.yaml list  0.22s user 0.15s system 85% cpu 0.438 total

	var p string
	info := &PkgInfo{
		Name: name,
	}

	// Check _only_ if this dep is in the current vendor directory.
	p = filepath.Join(r.VendorDir, filepath.FromSlash(name))
	if pkgExists(p) {
		info.Path = p
		info.Loc = LocVendor
		info.Vendored = true
		r.findCache[name] = info
		return info
	}

	// TODO: Do we need this if we always flatten?
	// Recurse backward to scan other vendor/ directories
	//for wd := cwd; wd != "/"; wd = filepath.Dir(wd) {
	//p = filepath.Join(wd, "vendor", filepath.FromSlash(name))
	//if fi, err = os.Stat(p); err == nil && (fi.IsDir() || isLink(fi)) {
	//info.Path = p
	//info.PType = ptypeVendor
	//info.Vendored = true
	//return info
	//}
	//}

	// Check $GOPATH
	for _, rr := range filepath.SplitList(r.BuildContext.GOPATH) {
		p = filepath.Join(rr, "src", filepath.FromSlash(name))
		if pkgExists(p) {
			info.Path = p
			info.Loc = LocGopath
			r.findCache[name] = info
			return info
		}
	}

	// Check $GOROOT
	for _, rr := range filepath.SplitList(r.BuildContext.GOROOT) {
		p = filepath.Join(rr, "src", filepath.FromSlash(name))
		if pkgExists(p) {
			info.Path = p
			info.Loc = LocGoroot
			r.findCache[name] = info
			return info
		}
	}

	// Finally, if this is "C", we're dealing with cgo
	if name == "C" {
		info.Loc = LocCgo
		r.findCache[name] = info
	}

	return info
}

func pkgExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && (fi.IsDir() || isLink(fi))
}

// isLink returns true if the given FileInfo is a symbolic link.
func isLink(fi os.FileInfo) bool {
	return fi.Mode()&os.ModeSymlink == os.ModeSymlink
}

func srcDir(fi os.FileInfo) bool {
	if !fi.IsDir() {
		return false
	}

	// Ignore _foo and .foo
	if strings.HasPrefix(fi.Name(), "_") || strings.HasPrefix(fi.Name(), ".") {
		return false
	}

	// Ignore testdata. For now, ignore vendor.
	if fi.Name() == "testdata" || fi.Name() == "vendor" {
		return false
	}

	return true
}
