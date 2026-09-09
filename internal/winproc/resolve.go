//go:build windows

package winproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultPathExt is what Windows falls back to when PATHEXT is not set.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// ResolveExecutable turns argv[0] into the absolute path CreateProcess needs.
//
// lpApplicationName is not resolved the way anyone expects it to be. A relative
// path there is resolved against the *calling* process's current directory --
// lpCurrentDirectory sets the child's working directory and has no bearing on
// finding the executable -- and when it is given, no PATH search happens at all.
//
// Both halves of that bite here. An egg whose startup command names a binary
// relative to the server's own files, which is the normal shape for a game
// server, had it looked for beside the worker instead: the worker's current
// directory is the instance directory, one level above the server's data, so
// "StarRupture\Binaries\Win64\Server.exe" was sought at
// E:\data\<uuid>\StarRupture\... rather than E:\data\<uuid>\data\StarRupture\...
// and reported as "The system cannot find the path specified". A bare name like
// "java" was never found at all.
//
// Passing NULL for lpApplicationName and letting CreateProcess parse the command
// line would restore the PATH search, but it also restores the ambiguity that
// makes an unquoted path containing spaces run the wrong program -- exactly the
// hazard that makes passing the executable explicitly worth doing. Resolving it
// here keeps the explicit form and fixes the lookup.
//
// env is the environment being handed to the child, so a PATH assembled for the
// egg's runtime is the one searched, rather than the worker's own.
func ResolveExecutable(argv0, dir string, env []string) (string, error) {
	if argv0 == "" {
		return "", fmt.Errorf("winproc: no executable specified")
	}

	exts := extensions(lookupEnv(env, "PATHEXT"))

	if filepath.IsAbs(argv0) {
		if found, ok := firstExisting(argv0, exts); ok {
			return found, nil
		}
		return "", fmt.Errorf("winproc: %s does not exist", argv0)
	}

	// Anything with a separator is relative to the server's own directory, which
	// is what the startup command in an egg means by it.
	if strings.ContainsAny(argv0, `\/`) {
		full := filepath.Join(dir, argv0)
		if found, ok := firstExisting(full, exts); ok {
			return found, nil
		}
		return "", fmt.Errorf("winproc: %s does not exist (%q resolved against the "+
			"server's directory %s)", full, argv0, dir)
	}

	// A bare name searches the server's directory first and then the PATH the
	// child is being given.
	dirs := append([]string{dir}, filepath.SplitList(lookupEnv(env, "PATH"))...)
	for _, base := range dirs {
		if base == "" {
			continue
		}
		if found, ok := firstExisting(filepath.Join(base, argv0), exts); ok {
			return found, nil
		}
	}
	return "", fmt.Errorf("winproc: %q was not found in the server's directory %s "+
		"or anywhere on the PATH it is being given", argv0, dir)
}

// firstExisting returns the first of path and path+ext that is a file.
func firstExisting(path string, exts []string) (string, bool) {
	if filepath.Ext(path) != "" {
		if isFile(path) {
			return path, true
		}
		// A name that already has an extension can still be a bare name that
		// happens to contain a dot, so the extensions are tried anyway.
	}
	for _, ext := range exts {
		if isFile(path + ext) {
			return path + ext, true
		}
	}
	if filepath.Ext(path) == "" && isFile(path) {
		return path, true
	}
	return "", false
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func extensions(pathext string) []string {
	if strings.TrimSpace(pathext) == "" {
		pathext = defaultPathExt
	}
	var out []string
	for _, e := range strings.Split(pathext, ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

// lookupEnv reads a variable from an environment block, case-insensitively as
// Windows treats them.
func lookupEnv(env []string, name string) string {
	for _, v := range env {
		if k, val, ok := strings.Cut(v, "="); ok && strings.EqualFold(k, name) {
			return val
		}
	}
	return ""
}
