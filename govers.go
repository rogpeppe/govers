/*
The govers command searches all Go packages under the current
directory for imports with a prefix matching a particular pattern, and
changes them to another specified prefix. As with gofmt and gofix, there is
no backup - you are expected to be using a version control system.
It prints the names of any packages that are modified.

Usage:

	govers [-d] [-m regexp] [-n] new-package-path

It accepts the following flags:

	-d
		Suppress dependency checking
	-m regexp
		Search for and change imports which have the
		given pattern as a prefix (see below for the default).
	-n
		Don't make any changes; just perform checks.

If the pattern is not specified with the -m flag, it is derived from
new-package-path and matches any prefix that is the same in all but
version.  A version is defined to be an element within a package path
that matches the regular expression "(/|\.)v[0-9.]+".

The govers command will also check (unless the -d flag is given)
that no (recursive) dependencies would be changed if the same govers
command was run on them. If they would, govers will fail and do nothing.

For example, say a new version of the tomb package is released.
The old import path was gopkg.in/tomb.v2, and we want
to use the new verson, gopkg.in/tomb.v3. In the root of the
source tree we want to change, we run:

	govers gopkg.in/tomb.v3

This will change all gopkg.in/tomb.v2 imports to use v3.
It will also check that all external packages that we're
using are also using v3, making sure that our program
is consistently using the same version throughout.

BUG: Vendored imports are not dealt with correctly - they won't
be changed. It's not yet clear how this command should work then.
*/
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const help = `
The govers command searches all Go packages under the current
directory for imports with a prefix matching a particular pattern, and
changes them to another specified prefix. As with gofmt and gofix, there is
no backup - you are expected to be using a version control system.
It prints the names of any packages that are modified.

Usage:

	govers [-d] [-m regexp] [-n] new-package-path

It accepts the following flags:

	-d
		Suppress dependency checking
	-m regexp
		Search for and change imports which have the
		given pattern as a prefix (see below for the default).
	-n
		Don't make any changes; just perform checks.

If the pattern is not specified with the -m flag, it is derived from
new-package-path and matches any prefix that is the same in all but
version.  A version is defined to be an element within a package path
that matches the regular expression "(/|\.)v[0-9.]+(-unstable)?".

The govers command will also check (unless the -d flag is given)
that no (recursive) dependencies would be changed if the same govers
command was run on them. If they would, govers will fail and do nothing.

For example, say a new version of the tomb package is released.
The old import path was gopkg.in/tomb.v2, and we want
to use the new verson, gopkg.in/tomb.v3. In the root of the
source tree we want to change, we run:

	govers gopkg.in/tomb.v3

This will change all gopkg.in/tomb.v2 imports to use v3.
It will also check that all external packages that we're
using are also using v3, making sure that our program
is consistently using the same version throughout.
`

var (
	match          = flag.String("m", "", "change imports with a matching prefix")
	noEdit         = flag.Bool("n", false, "don't make any changes; perform checks only")
	noDependencies = flag.Bool("d", false, "suppress dependency checking")
)

var cwd, _ = os.Getwd()

func main() {
	flag.Usage = func() {
		fmt.Printf("%s", help[1:])
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}
	newPackage := flag.Arg(0)
	cwd, err := os.Getwd()
	if err != nil {
		fatalf("cannot get working directory: %v", err)
	}
	var oldPackagePat *regexp.Regexp
	if *match != "" {
		oldPackagePat, err = regexp.Compile("^(" + *match + ")")
		if err != nil {
			fatalf("invalid match pattern: %v", err)
		}
	} else {
		oldPackagePat = pathVersionPat(newPackage)
	}
	buildCtxt := build.Default
	// BUG we ignore files that are ignored by the current build context
	// if we don't set this flag, but if we do set it, the import fails.
	// The solution is to avoid using build.Import but it's convenient
	// at the moment.
	//	buildCtxt.UseAllFiles = true
	ctxt := &context{
		cwd:           cwd,
		newPackage:    newPackage,
		oldPackagePat: oldPackagePat,
		buildCtxt:     &buildCtxt,
		checked:       make(map[string]bool),
		editPkgs:      make(map[string]*editPkg),
	}
	ctxt.walkDir(cwd)
	for path := range ctxt.editPkgs {
		ctxt.checkPackage(path, cwd)
	}
	if ctxt.failed {
		os.Exit(1)
	}
	for path, ep := range ctxt.editPkgs {
		if !ep.needsEdit {
			continue
		}
		changed := false
		for _, file := range ep.goFiles {
			changed = ctxt.changeVersion(file) || changed
		}
		if changed {
			fmt.Printf("%s\n", path)
		}
	}
	if ctxt.failed {
		os.Exit(1)
	}
}

type editPkg struct {
	goFiles   []string
	needsEdit bool
}

type context struct {
	cwd           string
	failed        bool
	newPackage    string
	oldPackagePat *regexp.Regexp
	buildCtxt     *build.Context
	checked       map[string]bool
	editPkgs      map[string]*editPkg
}

// walkDir walks all directories below path and
// adds any packages to ctxt.editPkgs.
func (ctxt *context) walkDir(path string) {
	entries, err := ioutil.ReadDir(path)
	if err != nil {
		logf("cannot read directory %q: %v", path, err)
		return
	}
	var ep editPkg
	for _, entry := range entries {
		if entry.IsDir() {
			if !strings.HasPrefix(entry.Name(), ".") {
				ctxt.walkDir(filepath.Join(path, entry.Name()))
			}
		} else {
			if strings.HasSuffix(entry.Name(), ".go") {
				ep.goFiles = append(ep.goFiles, filepath.Join(path, entry.Name()))
			}
		}
	}
	pkg, err := ctxt.buildCtxt.Import(".", path, build.FindOnly)
	if err != nil {
		// ignore directories that don't correspond to packages.
		return
	}
	ctxt.editPkgs[pkg.ImportPath] = &ep
}

// checkPackage checks all go files in the given
// package, and all their dependencies.
func (ctxt *context) checkPackage(path, fromDir string) {
	if path == "C" {
		return
	}
	if ctxt.checked[path] {
		// The package has already been, is or being, checked
		return
	}
	pkg, err := ctxt.buildCtxt.Import(path, fromDir, 0)
	ctxt.checked[pkg.ImportPath] = true
	if err != nil {
		if _, ok := err.(*build.NoGoError); !ok {
			logf("cannot import %q from %q: %v", path, fromDir, err)
		}
		return
	}
	ep := ctxt.editPkgs[path]
	// N.B. is it worth eliminating duplicates here?
	var allImports []string
	allImports = append(allImports, pkg.Imports...)
	if ctxt.editPkgs[path] != nil {
		// The package is in our set of root packages so
		// add testing imports too.
		allImports = append(allImports, pkg.TestImports...)
		allImports = append(allImports, pkg.XTestImports...)
	}
	for _, impPath := range allImports {
		// Import the package to find out its absolute path
		// including vendor directories before applying the
		// rewrite.
		impPkg, _ := ctxt.buildCtxt.Import(impPath, pkg.Dir, 0)
		if err != nil {
			continue
		}
		if p := ctxt.fixPath(impPkg.ImportPath); p != impPkg.ImportPath {
			if ep == nil {
				logf("package %q is using inconsistent path %q", pkg.ImportPath, impPkg.ImportPath)
				ctxt.failed = true
				continue
			}
			ep.needsEdit = true
			impPath = p
		}
		if !*noDependencies {
			ctxt.checkPackage(impPath, impPkg.Dir)
		}
	}
}

var printConfig = printer.Config{
	Mode:     printer.TabIndent | printer.UseSpaces,
	Tabwidth: 8,
}

// changeVersion changes the named go file to
// import the new version.
func (ctxt *context) changeVersion(path string) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		logf("cannot parse %q: %v", path, err)
	}
	changed := false
	for _, ispec := range f.Imports {
		impPath, err := strconv.Unquote(ispec.Path.Value)
		if err != nil {
			panic(err)
		}
		if p := ctxt.fixPath(impPath); p != impPath {
			ispec.Path.Value = strconv.Quote(p)
			changed = true
		}
	}
	if !changed || *noEdit {
		return changed
	}
	out, err := os.Create(path)
	if err != nil {
		logf("cannot create file: %v", err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	if err := printConfig.Fprint(w, fset, f); err != nil {
		logf("cannot write file: %v", err)
	}
	if err := w.Flush(); err != nil {
		logf("cannot write file: %v", err)
	}
	return true
}

func (ctxt *context) fixPath(p string) string {
	loc := ctxt.oldPackagePat.FindStringSubmatchIndex(p)
	if loc == nil {
		return p
	}
	i := loc[3]
	if p[0:i] != ctxt.newPackage {
		p = ctxt.newPackage + p[i:]
	}
	return p
}

const versPat = `[/|\.]v[0-9]+(-unstable)?`

// pathVersionPat returns a pattern that will match any
// package path that's the same except possibly
// the version number.
func pathVersionPat(p string) *regexp.Regexp {
	versRe := regexp.MustCompile(versPat + "(/|$)")
	if !versRe.MatchString(p) {
		fatalf("%q is not versioned", p)
	}
	// Use an intermediate step so that we can use QuoteMeta after
	// matching against versPat.  (versPat won't match quoted
	// metacharacters).
	// Note that  '#' is an invalid character in an import path
	p = versRe.ReplaceAllString(p, "#")
	p = regexp.QuoteMeta(p)
	// BUG doesn't match "foo/v0/v1/bar", but do we care?
	p = "^(" + strings.Replace(p, "#", versPat, -1) + ")(/|$)"
	return regexp.MustCompile(p)
}

func logf(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "govers: %s\n", fmt.Sprintf(f, a...))
}

func fatalf(f string, a ...interface{}) {
	logf(f, a...)
	os.Exit(2)
}
