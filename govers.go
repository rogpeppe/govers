// The govers command edits all packages
// below the current directory to use the
// given package path prefix. As with gofmt
// and gofix, there is no backup - you are expected
// to be using a version control system.
//
// A versioned package path is defined to be any
// path containing an element that matches the regular
// expression "v[0-9.]+".
//
// Any import that has the given prefix will be changed.
//
// The govers command will also check that all dependencies
// use the same version; if they do not, it will fail and do nothing.
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

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: govers new-package-path\n")
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}
	cwd, err := os.Getwd()
	if err != nil {
		fatalf("cannot get working directory: %v", err)
	}
	buildCtxt := build.Default
	// BUG we ignore files that are ignored by the current build context
	// if we don't set this flag, but if we do set it, the import fails.
	// The solution is to avoid using build.Import but it's convenient
	// at the moment.
	//	buildCtxt.UseAllFiles = true
	ctxt := &context{
		cwd:           cwd,
		fixPackage:    flag.Arg(0),
		fixPackagePat: pathPat(flag.Arg(0)),
		buildCtxt:     &buildCtxt,
		checked:       make(map[string]bool),
		editPkgs:      make(map[string]*editPkg),
	}
	ctxt.walkDir(cwd)
	for path := range ctxt.editPkgs {
		ctxt.checkPackage(path)
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
	fixPackage    string
	fixPackagePat *regexp.Regexp
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
func (ctxt *context) checkPackage(path string) {
	if path == "C" {
		return
	}
	if ctxt.checked[path] {
		// The package has already been, is or being, checked
		return
	}
	pkg, err := ctxt.buildCtxt.Import(path, ".", 0)
	ctxt.checked[pkg.ImportPath] = true
	if err != nil {
		if _, ok := err.(*build.NoGoError); !ok {
			logf("cannot import %q: %v", path, err)
		}
		return
	}
	ep := ctxt.editPkgs[path]
	for _, impPath := range pkg.Imports {
		if p := ctxt.fixPath(impPath); p != impPath {
			if ep == nil {
				logf("%q uses %q", path, impPath)
				ctxt.failed = true
				continue
			}
			ep.needsEdit = true
			impPath = p
		}
		ctxt.checkPackage(impPath)
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
	if !changed {
		return false
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
	loc := ctxt.fixPackagePat.FindStringSubmatchIndex(p)
	if loc == nil {
		return p
	}
	i := loc[3]
	if p[0:i] != ctxt.fixPackage {
		p = ctxt.fixPackage + p[i:]
	}
	return p
}

const versPat = `/v([0-9.)]+`

// pathPat returns a pattern that will match any
// package path that's the same except possibly
// the version number.
func pathPat(p string) *regexp.Regexp {
	versRe := regexp.MustCompile(versPat + "(/|$)")
	if !versRe.MatchString(p) {
		fatalf("%q is not versioned", p)
	}
	p = regexp.QuoteMeta(p)
	// BUG doesn't match "foo/v0/v1/bar", but do we care?
	p = "^(" + versRe.ReplaceAllString(p, versPat) + ")(/|$)"
	return regexp.MustCompile(p)
}

func logf(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "govers: %s\n", fmt.Sprintf(f, a...))
}

func fatalf(f string, a ...interface{}) {
	logf(f, a...)
	os.Exit(2)
}
