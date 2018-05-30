package get

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dave/services/getter/gettermsg"
)

func (g *Getter) download(ctx context.Context, path string, parent *Package, stk *ImportStack, update bool, insecure, single bool) error {
	load1 := func(path string, useVendor bool) *Package {
		if parent == nil {
			return g.LoadImport(ctx, path, "/", nil, stk, false)
		}
		return g.LoadImport(ctx, path, parent.Dir, parent, stk, useVendor)
	}
	p := load1(path, false)
	if p.Error != nil && p.Error.Hard {
		return p.Error
	}

	// loadPackage inferred the canonical ImportPath from arg.
	// Use that in the following to prevent hysteresis effects
	// in e.g. downloadCache and packageCache.
	// This allows invocations such as:
	//   mkdir -p $GOPATH/src/github.com/user
	//   cd $GOPATH/src/github.com/user
	//   go get ./foo
	// see: golang.org/issue/9767
	path = p.ImportPath

	// There's nothing to do if this is a package in the standard library.
	if p.Standard {
		return nil
	}

	// Only process each package once.
	// (Unless we're fetching test dependencies for this package,
	// in which case we want to process it again.)
	if g.downloadCache[path] {
		return nil
	}
	if !single {
		// don't set the download cache in single mode, because we want to continue processing imports
		// if we download this package again e.g. in Initialise mode
		g.downloadCache[path] = true
	}

	pkgs := []*Package{p}

	// Download if the package is missing, or update if we're using -u.
	if p.Dir == "" || update {
		// The actual download.
		stk.Push(path)
		err := g.downloadPackage(ctx, p, update, insecure)
		if err != nil {
			perr := &PackageError{ImportStack: stk.Copy(), Err: err.Error()}
			stk.Pop()
			return perr
		}
		stk.Pop()

		// Clear all relevant package cache entries before
		// doing any new loads.
		g.ClearPackageCachePartial([]string{path})

		pkgs = pkgs[:0]

		// Note: load calls loadPackage or loadImport,
		// which push arg onto stk already.
		// Do not push here too, or else stk will say arg imports arg.
		p := load1(path, false)
		if p.Error != nil {
			return p.Error
		}
		pkgs = append(pkgs, p)

	} else {
		// if we're not downloading the repo, then work out what the repo is and store this in repoPackages
		// so it can be stored as a hint by gitcache
		if root, _ := g.vcsFromDir(p.Dir, p.Internal.Build.SrcRoot); root != nil {
			// ignore the error
			g.repoPackages[p.ImportPath] = root
		}
	}

	// single mode - don't process dependencies
	if single {
		return nil
	}

	// Process package, which might now be multiple packages
	// due to wildcard expansion.
	for _, p := range pkgs {
		// Process dependencies, now that we know what they are.
		imports := p.Imports
		for i, path := range imports {
			if path == "C" {
				continue
			}
			// Fail fast on import naming full vendor path.
			// Otherwise expand path as needed for test imports.
			// Note that p.Imports can have additional entries beyond p.Internal.Build.Imports.
			orig := path
			if i < len(p.Internal.Build.Imports) {
				orig = p.Internal.Build.Imports[i]
			}
			if j, ok := FindVendor(orig); ok {
				stk.Push(path)
				err := &PackageError{
					ImportStack: stk.Copy(),
					Err:         "must be imported as " + path[j+len("vendor/"):],
				}
				stk.Pop()
				return err
			}
			// If this is a test import, apply vendor lookup now.
			// We cannot pass useVendor to download, because
			// download does caching based on the value of path,
			// so it must be the fully qualified path already.
			if i >= len(p.Imports) {
				path = g.VendoredImportPath(p, path)
			}
			if err := g.download(ctx, path, p, stk, update, insecure, false); err != nil {
				return err
			}
		}
	}

	return nil
}

// downloadPackage runs the create or download command
// to make the first copy of or update a copy of the given package.
func (g *Getter) downloadPackage(ctx context.Context, p *Package, update bool, insecure bool) error {

	var (
		root *repoRoot
		err  error
	)

	if p.Internal.Build.SrcRoot != "" {
		// Directory exists. Look for checkout along path to src.
		root, err = g.vcsFromDir(p.Dir, p.Internal.Build.SrcRoot)
		if err != nil {
			return err
		}
	} else {
		// Analyze the import path to determine the version control system,
		// repository, and the import path for the root of the repository.
		root, err = g.repoRootForImportPath(ctx, p.ImportPath, insecure)
		if err != nil {
			return err
		}
	}
	if !isSecure(root.repo) && !insecure {
		return fmt.Errorf("cannot download, %v uses insecure protocol", root.repo)
	}

	if p.Internal.Build.SrcRoot == "" {
		// Package not found. Put in first directory of $GOPATH.
		list := filepath.SplitList(g.buildContext.GOPATH)
		if len(list) == 0 {
			return fmt.Errorf("cannot download, $GOPATH not set. For more details see: 'go help gopath'")
		}
		// Guard against people setting GOPATH=$GOROOT.
		if filepath.Clean(list[0]) == filepath.Clean(g.buildContext.GOROOT) {
			return fmt.Errorf("cannot download, $GOPATH must not be set to $GOROOT. For more details see: 'go help gopath'")
		}
		if _, err := g.session.GoPath().Stat(filepath.Join(list[0], "src/cmd/go/alldocs.go")); err == nil {
			return fmt.Errorf("cannot download, %s is a GOROOT, not a GOPATH. For more details see: 'go help gopath'", list[0])
		}
		p.Internal.Build.Root = list[0]
		p.Internal.Build.SrcRoot = filepath.Join(list[0], "src")
		p.Internal.Build.PkgRoot = filepath.Join(list[0], "pkg")
	}
	dir := filepath.Join(p.Internal.Build.SrcRoot, filepath.FromSlash(root.root))
	if root.dir == "" {
		root.dir = dir
	} else if root.dir != dir {
		return fmt.Errorf("path disagreement, calculated %s, expected %s", dir, root.dir)
	}

	g.repoPackages[p.ImportPath] = root

	// If we've considered this repository already, don't do it again.
	if _, ok := g.downloadRootCache[root.dir]; ok {
		return nil
	}
	g.downloadRootCache[root.dir] = root

	if !root.exists {
		fs := g.session.GoPath()

		// Root does not exist. Prepare to checkout new copy.
		// Some version control tools require the target directory not to exist.
		// We require that too, just to avoid stepping on existing work.
		if _, err := fs.Stat(root.dir); err == nil {
			return fmt.Errorf("%s exists but repo does not", root.dir)
		}

		// Some version control tools require the parent of the target to exist.
		parent, _ := filepath.Split(root.dir)
		if err = fs.MkdirAll(parent, 0777); err != nil {
			return err
		}
		if g.send != nil {
			g.send(gettermsg.Downloading{Message: root.root})
		}

		if err = root.create(ctx, fs); err != nil {
			return err
		}
	} else {
		// Root does exist; download incremental updates.
		panic("root exists")

		if g.send != nil {
			g.send(gettermsg.Downloading{Message: root.root})
		}

		if err = root.download(ctx); err != nil {
			return err
		}
	}

	//if cfg.BuildN {
	// Do not show tag sync in -n; it's noise more than anything,
	// and since we're not running commands, no tag will be found.
	// But avoid printing nothing.
	//	fmt.Fprintf(os.Stderr, "# cd %s; %s sync/update\n", rootDir, vcs.cmd)
	//	return nil
	//}

	// TODO: work out if we actually need this...

	// Select and sync to appropriate version of the repository.
	//tags, err := vcs.tags(rootDir)
	//if err != nil {
	//	return err
	//}
	//vers := runtime.Version()
	//if i := strings.Index(vers, " "); i >= 0 {
	//	vers = vers[:i]
	//}
	//if err := vcs.tagSync(rootDir, selectTag(vers, tags)); err != nil {
	//	return err
	//}

	return nil
}
