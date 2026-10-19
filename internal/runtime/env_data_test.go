package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// piEnvLoad and codexEnvLoad are the .env reads the pi and codex launches
// emit where they used to source the file.
var (
	piEnvLoad    = envDataLoad(sandbox.SandboxWorkspace+"/.env", piBinaryVar)
	codexEnvLoad = envDataLoad(sandbox.SandboxWorkspace+"/.env", codexBinaryVar, codexPathVar)
)

// envDataFixture is a temporary workspace with a .env, an .env.d directory
// and an iteration.env, loaded the way the launches load the real one.
type envDataFixture struct {
	ws      string
	envFile string
}

func newEnvDataFixture(t *testing.T) envDataFixture {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".env.d"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".fullsend"), 0o755))
	return envDataFixture{ws: ws, envFile: filepath.Join(ws, ".env")}
}

func (f envDataFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.ws, rel), []byte(content), 0o644))
}

// load runs the loader, then tail (a shell snippet), under /bin/sh with a
// minimal environment, and returns stdout and stderr.
func (f envDataFixture) load(t *testing.T, tail string, extraDrop ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", envDataLoadWith(f.envFile, envDataControlLines(f.ws), extraDrop)+" && "+tail)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/agent"}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// envOf returns the exported environment the shell has after the load.
func (f envDataFixture) envOf(t *testing.T, extraDrop ...string) (map[string]string, string) {
	t.Helper()
	out, stderr, err := f.load(t, "env -0", extraDrop...)
	require.NoError(t, err, stderr)
	env := map[string]string{}
	for kv := range strings.SplitSeq(out, "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, stderr
}

// The proposed test from the issue: no shell construct in .env executes at
// launch, LD_* never reaches the CLI, and a PATH prepend survives.
func TestEnvDataLoad_HostileEnvDoesNotExecute(t *testing.T) {
	f := newEnvDataFixture(t)
	marker := filepath.Join(f.ws, "pwned")
	hostile := strings.Join([]string{
		"export PATH=/sandbox/workspace/bin:$PATH",
		"export SUB=$(touch " + marker + ")",
		"export SUB2=\"$(touch " + marker + ")\"",
		"export TICK=`touch " + marker + "`",
		"export TICK2=\"`touch " + marker + "`\"",
		"claude() { touch " + marker + "; }",
		"touch " + marker,
		". " + marker,
		"exec /bin/true",
		"A=1; touch " + marker,
		"B=1 touch " + marker,
		"C=1 && touch " + marker,
		"D=x | touch " + marker,
		"export DFL=${UNSET:-$(touch " + marker + ")}",
		"export LD_PRELOAD=/tmp/evil.so",
		"export LD_LIBRARY_PATH=/tmp/evil",
		"LD_AUDIT=/tmp/evil.so",
		"export LD_AUDIT",
		"export IFS=/",
		"export FULLSEND_TEST_PIN=/evil",
		"readonly KEPT=x",
		"alias ls='touch " + marker + "'",
		"export KEPT=ok",
	}, "\n") + "\n"
	f.write(t, ".env", hostile)

	env, stderr := f.envOf(t, "FULLSEND_TEST_PIN")
	assert.NoFileExists(t, marker, "a construct from .env ran")
	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "IFS", "FULLSEND_TEST_PIN", "SUB", "SUB2", "TICK", "TICK2", "DFL"} {
		_, ok := env[name]
		assert.False(t, ok, "%s was taken from .env", name)
	}
	assert.True(t, strings.HasPrefix(env["PATH"], "/sandbox/workspace/bin:"), "the PATH prepend survives: %s", env["PATH"])
	assert.Equal(t, "ok", env["KEPT"])
	assert.NotContains(t, stderr, marker, "dropped lines are reported by number, not content")
	assert.Contains(t, stderr, "line 2:")
}

// The values written by a readonly pin cannot abort the launch: the
// assignment is dropped before the shell sees it.
func TestEnvDataLoad_ExtraDropKeepsPinnedValue(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", "export FULLSEND_TEST_PIN=/evil\n")
	out, stderr, err := f.load(t, `printf '%s' "$FULLSEND_TEST_PIN"`, "FULLSEND_TEST_PIN")
	require.NoError(t, err, stderr)
	assert.Empty(t, out)
	assert.Contains(t, stderr, "FULLSEND_TEST_PIN is not taken from .env")
}

// A .env shaped like the one internal/cli writes, with .env.d files and
// iteration.env, loads to the values sourcing it gave.
func TestEnvDataLoad_RunnerShapedEnv(t *testing.T) {
	f := newEnvDataFixture(t)
	control := envDataControlLines(f.ws)
	f.write(t, ".env", strings.Join([]string{
		"export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH:" + f.ws + "/bin",
		control[0],
		"export CLAUDE_CONFIG_DIR=/sandbox/claude-config",
		"export FULLSEND_OUTPUT_FILE='it'\\''s.json'",
		control[1],
		"export ENV_SANDBOX='multi",
		"line'",
		control[2],
		control[3],
		"export TIRITH_REQUIRED=1",
	}, "\n")+"\n")
	f.write(t, ".env.d/b.env", "export ORDER=b\nexport PATH=/envd/bin:$PATH\n")
	f.write(t, ".env.d/a.env", "export ORDER=a\nexport VERTEX='x y'\n# comment\nPLAIN=notexported\n")
	f.write(t, ".env.d/.hidden.env", "export HIDDEN=1\n")
	f.write(t, ".env.d/ignored.txt", "export IGNORED=1\n")
	f.write(t, ".fullsend/iteration.env", "export FULLSEND_TIMEOUT_MINUTES=30\nexport TRACEPARENT=\n")

	env, stderr := f.envOf(t)
	assert.Empty(t, stderr)
	runnerPath := "/usr/local/go/bin:/home/agent/go/bin:" + os.Getenv("PATH") + ":" + f.ws + "/bin"
	assert.Equal(t, runnerPath+":/envd/bin:"+runnerPath, env["PATH"], "the runner's PATH goes back in front of an .env.d prepend")
	assert.Equal(t, "/sandbox/claude-config", env["CLAUDE_CONFIG_DIR"])
	assert.Equal(t, "it's.json", env["FULLSEND_OUTPUT_FILE"])
	assert.Equal(t, "multi\nline", env["ENV_SANDBOX"])
	assert.Equal(t, "b", env["ORDER"], ".env.d files are read in name order")
	assert.Equal(t, "x y", env["VERTEX"])
	assert.Equal(t, "30", env["FULLSEND_TIMEOUT_MINUTES"])
	assert.Equal(t, "1", env["TIRITH_REQUIRED"])
	v, ok := env["TRACEPARENT"]
	assert.True(t, ok)
	assert.Empty(t, v)
	for _, name := range []string{"PLAIN", "HIDDEN", "IGNORED", "FULLSEND_RUNNER_PATH"} {
		_, ok := env[name]
		assert.False(t, ok, "%s is not exported", name)
	}
}

// Without the iteration.env and .env.d files the control lines read nothing.
func TestEnvDataLoad_ControlLinesWithoutFiles(t *testing.T) {
	f := newEnvDataFixture(t)
	require.NoError(t, os.RemoveAll(filepath.Join(f.ws, ".env.d")))
	control := envDataControlLines(f.ws)
	f.write(t, ".env", strings.Join(control, "\n")+"\n"+control[0]+"\n")
	env, stderr := f.envOf(t)
	assert.Empty(t, stderr)
	assert.Equal(t, os.Getenv("PATH")+":"+os.Getenv("PATH"), env["PATH"])
}

func TestEnvDataLoad_ValueGrammar(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", strings.Join([]string{
		`export DQ="a \$b ${HOME} \"q\" \\ \x"`,
		`export CAT='one'"two"three$HOME`,
		`export TILDE=~/bin:~:/x`,
		`QUOTED_TILDE='~'`,
		`export QUOTED_TILDE`,
		`export ESC=a\ b`,
		`export EMPTY=`,
		`export HASH=a#b # trailing comment`,
		`  export INDENTED=yes`,
		`export DQ_MULTI="l1`,
		`l2"`,
		`export A=1 B=2`,
		`export UNTERMINATED='x`,
	}, "\n")+"\n")
	env, stderr := f.envOf(t)
	assert.Equal(t, `a $b /home/agent "q" \ \x`, env["DQ"])
	assert.Equal(t, "onetwothree/home/agent", env["CAT"])
	assert.Equal(t, "/home/agent/bin:/home/agent:/x", env["TILDE"])
	assert.Equal(t, "~", env["QUOTED_TILDE"])
	assert.Equal(t, "a b", env["ESC"])
	assert.Equal(t, "a#b", env["HASH"])
	assert.Equal(t, "yes", env["INDENTED"])
	assert.Equal(t, "l1\nl2", env["DQ_MULTI"])
	v, ok := env["EMPTY"]
	assert.True(t, ok)
	assert.Empty(t, v)
	for _, name := range []string{"A", "B", "UNTERMINATED"} {
		_, ok := env[name]
		assert.False(t, ok, "%s", name)
	}
	assert.Contains(t, stderr, "line 12: trailing text after the assignment")
	assert.Contains(t, stderr, "line 13: unterminated quote")
}

func TestEnvDataLoad_RejectedValues(t *testing.T) {
	f := newEnvDataFixture(t)
	for _, line := range []string{
		`export X=~root/bin`,
		`export X=${HOME:-x}`,
		`export X=$`,
		`export X="${HOME"`,
		`export X=a\`,
		`export X="unterminated`,
		`export X=(a)`,
		`export 1X=a`,
		`export X Y-Z`,
		`X`,
	} {
		f.write(t, ".env", line+"\n")
		env, stderr := f.envOf(t)
		_, ok := env["X"]
		assert.False(t, ok, "%s", line)
		assert.Contains(t, stderr, "line 1:", "%s", line)
	}
}

// `export NAME` exports a value assigned earlier or later, as in a shell.
func TestEnvDataLoad_ExportMarksNames(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", "BEFORE=1\nexport BEFORE AFTER\nAFTER=2\nexport HOME=/h\nNEVER=3\n")
	env, stderr := f.envOf(t)
	assert.Empty(t, stderr)
	assert.Equal(t, "1", env["BEFORE"])
	assert.Equal(t, "2", env["AFTER"])
	assert.Equal(t, "/h", env["HOME"])
	_, ok := env["NEVER"]
	assert.False(t, ok)
}

// A plain NAME=value stays a shell variable of the launch shell, so a later
// launch line that expands it sees it (as it did when .env was sourced), but
// it does not reach the CLI's environment.
func TestEnvDataLoad_NonExportedIsShellVariable(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", "PLAIN_ONLY='it'\\''s here'\nexport MARKED=1\nREADY=$PLAIN_ONLY\n")
	out, stderr, err := f.load(t, `printf '%s|%s' "${PLAIN_ONLY:-unset}" "${READY:-unset}"`)
	require.NoError(t, err, stderr)
	assert.Equal(t, "it's here|it's here", out)
	env, _ := f.envOf(t)
	assert.Equal(t, "1", env["MARKED"])
	for _, name := range []string{"PLAIN_ONLY", "READY"} {
		_, ok := env[name]
		assert.False(t, ok, "%s is not exported", name)
	}
}

// A missing .env fails the launch, as sourcing a missing file did.
func TestEnvDataLoad_MissingEnvFails(t *testing.T) {
	f := newEnvDataFixture(t)
	out, stderr, err := f.load(t, "echo LAUNCHED")
	require.Error(t, err)
	assert.NotContains(t, out, "LAUNCHED")
	assert.Contains(t, stderr, "cannot read")
}

// An .env.d file or iteration.env that exists but cannot be read fails the
// launch before any export is emitted, instead of losing its settings.
func TestEnvDataLoad_UnreadableIncludedFileFails(t *testing.T) {
	for _, rel := range []string{".env.d/a.env", ".fullsend/iteration.env"} {
		t.Run(rel, func(t *testing.T) {
			f := newEnvDataFixture(t)
			f.write(t, ".env", strings.Join(envDataControlLines(f.ws), "\n")+"\nexport KEEP=1\n")
			f.write(t, rel, "export LOST=1\n")
			path := filepath.Join(f.ws, rel)
			require.NoError(t, os.Chmod(path, 0))
			if _, err := os.ReadFile(path); err == nil {
				t.Skip("file stays readable (running as root)")
			}
			out, stderr, err := f.load(t, "echo LAUNCHED")
			require.Error(t, err)
			assert.NotContains(t, out, "LAUNCHED")
			assert.Contains(t, stderr, "cannot read")
			assert.Contains(t, stderr, filepath.Base(rel))
		})
	}
}

// The loader names a file by its escaped form, so a newline in an
// agent-chosen .env.d file name cannot forge a separate log line.
func TestEnvDataLoad_PathsAreEscapedInMessages(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", strings.Join(envDataControlLines(f.ws), "\n")+"\n")
	f.write(t, ".env.d/x\n::error::forged.env", "$(id)\n")
	_, stderr, err := f.load(t, "true")
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "ignored")
	assert.Equal(t, 1, strings.Count(strings.TrimSpace(stderr), "\n")+1, "one line: %q", stderr)
	assert.NotContains(t, stderr, "\n::error::")
}

// Python startup variables are never taken from .env, so a planted
// PYTHONPATH cannot reach a python3 the CLI starts later.
func TestEnvDataLoad_DropsPythonStartupVariables(t *testing.T) {
	f := newEnvDataFixture(t)
	f.write(t, ".env", "export PYTHONPATH=/sandbox/workspace/evil\nexport PYTHONHOME=/x\nexport PYTHONSTARTUP=/x.py\nexport PYTHONUSERBASE=/x\nexport KEEP=1\n")
	env, stderr := f.envOf(t)
	assert.Equal(t, "1", env["KEEP"])
	for _, name := range []string{"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE"} {
		_, ok := env[name]
		assert.False(t, ok, "%s must not reach the CLI", name)
		assert.Contains(t, stderr, name+" is not taken from .env")
	}
}

// Shell startup variables are never taken from .env, so a planted
// BASH_ENV cannot run a file in a bash the CLI starts later.
func TestEnvDataLoad_DropsShellStartupVariables(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	f := newEnvDataFixture(t)
	marker := filepath.Join(f.ws, "pwned")
	f.write(t, "startup.sh", "touch "+marker+"\n")
	f.write(t, ".env", "export BASH_ENV="+filepath.Join(f.ws, "startup.sh")+"\nexport ENV="+filepath.Join(f.ws, "startup.sh")+"\nexport KEEP=1\n")
	env, stderr := f.envOf(t)
	assert.Equal(t, "1", env["KEEP"])
	for _, name := range []string{"BASH_ENV", "ENV"} {
		_, ok := env[name]
		assert.False(t, ok, "%s must not reach the CLI", name)
		assert.Contains(t, stderr, name+" is not taken from .env")
	}
	_, stderr, err := f.load(t, "bash -c true")
	require.NoError(t, err, stderr)
	assert.NoFileExists(t, marker, "a bash child must not run the .env BASH_ENV file")
}

// The loader script spells the runner-PATH variable out; keep it in step
// with the constant the control lines use.
func TestEnvDataLoaderScript_RunnerPathVar(t *testing.T) {
	assert.Contains(t, envDataLoaderScript, `"`+envDataRunnerPathVar+`"`)
}

// The fragment pins python3 before reading .env and runs it isolated.
func TestEnvDataLoad_Shape(t *testing.T) {
	frag := envDataLoad("/sandbox/workspace/.env", "FULLSEND_X_BIN")
	assert.True(t, strings.HasPrefix(frag, envDataPin()+" && "), frag)
	assert.Contains(t, frag, `"$`+envDataPythonVar+`" -I -S -c `)
	assert.Contains(t, frag, `eval "$`+envDataExportsVar+`" && unset `+envDataExportsVar)
	assert.Contains(t, frag, "'LD_PRELOAD LD_LIBRARY_PATH LD_AUDIT PYTHONPATH PYTHONHOME PYTHONSTARTUP PYTHONUSERBASE BASH_ENV ENV IFS "+envDataPythonVar+" "+envDataExportsVar+" FULLSEND_X_BIN'")
	assert.NotContains(t, frag, ". /sandbox/workspace/.env")
	assert.NotContains(t, frag, "  ", "the launch commands are checked for double spaces")
	for _, line := range EnvDataControlLines() {
		assert.Contains(t, frag, shellQuote(line))
	}
}
