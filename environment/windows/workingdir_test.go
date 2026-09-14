//go:build windows

package windows

import "testing"

// workingDirEnv builds an environment carrying a working directory override,
// with the server variables a profile field would be written against.
//
// Assembled directly rather than through New, for the same reason consoleEnv is:
// resolving this reads only the metadata.
func workingDirEnv(dir string) (*Environment, []string) {
	e := &Environment{Id: "working-dir-test", meta: &Metadata{WorkingDir: dir}}
	return e, []string{"SERVER_PORT=30000", "GAME=StarRupture"}
}

func TestResolveWorkingDirIsEmptyByDefault(t *testing.T) {
	// The ordinary egg. Empty has to survive as empty all the way to the worker,
	// which reads it as "the data directory" -- an egg that sets nothing must not
	// end up with a working directory computed for it.
	e, vars := workingDirEnv("")

	if got := e.resolveWorkingDir(vars); got != "" {
		t.Errorf("working dir = %q, want empty", got)
	}
}

func TestResolveWorkingDirSubstitutesTheServersVariables(t *testing.T) {
	e, vars := workingDirEnv(`{{GAME}}\Binaries`)

	if want, got := `StarRupture\Binaries`, e.resolveWorkingDir(vars); got != want {
		t.Errorf("working dir = %q, want %q", got, want)
	}
}

func TestResolveWorkingDirRefusesPathsThatLeaveTheServer(t *testing.T) {
	// The whole point of the field is to keep a game's own paths inside the
	// sandbox. One that climbs out itself would defeat it, and an absolute path
	// pasted from the game's documentation is the likely way to write one.
	for _, dir := range []string{
		`..\..\windows`,
		`ServerFile\..\..\elsewhere`,
		`C:\Games\Server`,
		`\\host\share`,
		`\absolute`,
		`/absolute`,
		`C:relative`,
	} {
		e, vars := workingDirEnv(dir)

		if got := e.resolveWorkingDir(vars); got != "" {
			t.Errorf("working dir %q resolved to %q, want it refused", dir, got)
		}
	}
}

func TestResolveWorkingDirRefusesAPathThatOnlyEscapesAfterSubstitution(t *testing.T) {
	// Checked after expansion, not before: a variable is the natural way to write
	// one of these, and the value only exists on the node.
	e, vars := workingDirEnv(`{{GAME}}`)
	vars = append(vars, `GAME=..\..\somewhere`)

	// The later assignment wins in envLookup, matching how the process
	// environment itself resolves a repeated key.
	if got := e.resolveWorkingDir(vars); got != "" {
		t.Errorf("working dir = %q, want it refused", got)
	}
}
