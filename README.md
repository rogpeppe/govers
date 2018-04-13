The govers command searches all Go packages under the current
directory for imports with a prefix matching a particular pattern, and
changes them to another specified prefix. As with gofmt and gofix, there is
no backup - you are expected to be using a version control system.
It prints the names of any packages that are modified.

Usage:

	govers [-d] [-m regexp] [tag tag_name] [-n] new-package-path

It accepts the following flags:

	-d
		Suppress dependency checking
	-m regexp
		Search for and change imports which have the
		given pattern as a prefix (see below for the default).
	-n
		Don't make any changes; just perform checks.
	-tags 'tag list'
		A space-separated list of build tags to satisfy when considering
		files to change.

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
