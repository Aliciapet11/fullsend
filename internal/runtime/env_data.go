package runtime

import (
	"strings"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

// The launch reads the agent-writable workspace .env as data rather than
// sourcing it as shell (#8498). Sourcing it ran agent-written code in the
// launch shell itself, before the launch-time checks had finished: a .env
// could rewrite a hook file and `exec` the CLI, so the later unsets, the
// second integrity pass and the pinned binary never ran.
//
// envDataLoad replaces `. .env` in the Claude Code, codex and pi launches. A
// python3 resolved before anything the agent wrote is consulted (envDataPin,
// the same `command -v` pin the CLIs use) runs isolated (-I: no PYTHON*
// variables, no user site, no script directory on sys.path; -S: no site
// module, so no .pth file runs), parses .env and the files it names, and
// prints one single-quoted `NAME='value'` line per variable, with `export `
// in front for the exported ones. The launch shell evals only that output, so
// no construct from .env executes.
//
// The grammar, per statement (a statement ends at a newline; a quoted value
// may span lines):
//
//   - blank lines and `#` comments;
//   - `[export ]NAME=VALUE` with an optional trailing `# comment`, where VALUE
//     concatenates single-quoted, double-quoted and unquoted text; `$NAME`
//     and `${NAME}` expand (unquoted or inside double quotes) to the value
//     known so far, and a leading `~` (or one after `:`) expands to $HOME;
//   - `export NAME...`, which marks names exported;
//   - the four runner-written control lines (EnvDataControlLines). Each
//     .env.d/*.env file (byte order) and iteration.env is read with the same
//     grammar, without control lines.
//
// Anything else (command substitution, functions, `.`, `exec`, `unset`,
// `readonly`, `alias`, `;`-joined commands, other parameter expansion forms)
// is dropped and reported on stderr by file and line only, never by content,
// since a dropped line can carry a credential.
//
// A name is exported when it was in the launch environment or is marked by
// `export`, as in a POSIX shell. Any other assignment is still set as a shell
// variable in the launch shell, so later launch lines that expand it see it,
// as they did when .env was sourced. Assignments to envDataDropNames and to the
// caller's extra names are dropped for every runtime: the dynamic-loader
// variables would load code into the CLI and every dynamically linked tool
// it starts, and the launch's own pins are read-only, so an assignment to
// one would abort the launch shell.
const (
	envDataPythonVar  = "FULLSEND_ENV_PYTHON"
	envDataExportsVar = "FULLSEND_ENV_EXPORTS"
	// envDataRunnerPathVar is the read-only variable the runner's PATH capture
	// line sets. The loader script spells the name out; a test keeps the two
	// in step.
	envDataRunnerPathVar = "FULLSEND_RUNNER_PATH"
)

// envDataDropNames are never taken from .env, for any runtime: the dynamic
// loader variables (#7580), the Python startup variables (a PYTHONPATH with a
// sitecustomize.py would run in every python3 the CLI starts, such as pi's
// hook adapter), the shell startup variables (an exported BASH_ENV runs a
// file in every non-interactive bash the CLI starts, ENV the same for an
// interactive sh), IFS (it would change how the rest of the launch splits
// words), and the loader's own variables.
var envDataDropNames = []string{
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
	"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE",
	"BASH_ENV", "ENV",
	"IFS",
	envDataPythonVar, envDataExportsVar,
}

// EnvDataControlLines are the runner-written .env lines the launch-time
// loader understands, in the order the runner writes them: the PATH
// capture, the .env.d loop, the PATH restore and the iteration.env read.
// internal/cli writes these exact lines (its tests check they agree) as
// shell for the agent's own shells; the loader interprets each as data.
func EnvDataControlLines() []string {
	return envDataControlLines(sandbox.SandboxWorkspace)
}

func envDataControlLines(workspace string) []string {
	iteration := workspace + "/.fullsend/iteration.env"
	return []string{
		`if ( ` + envDataRunnerPathVar + `= ) 2>/dev/null; then ` + envDataRunnerPathVar +
			`="$PATH"; readonly ` + envDataRunnerPathVar + `; fi`,
		`for f in ` + workspace + `/.env.d/*.env; do [ -f "$f" ] && . "$f"; done`,
		`export PATH="$` + envDataRunnerPathVar + `${PATH:+:$PATH}"`,
		"if [ -f " + iteration + " ]; then . " + iteration + "; fi",
	}
}

// envDataPin records where python3 is before anything the agent wrote can
// influence the lookup; see binaryPin.
func envDataPin() string {
	return binaryPin(envDataPythonVar, "python3")
}

// envDataLoad is the POSIX sh fragment that replaces `. envFile`. The
// assignment's status is the command substitution's, so a loader failure (a
// missing .env, a missing interpreter) ends the && chain. `eval` is a
// special builtin and nothing from .env has run, so no function can stand
// in for it, and it only ever sees the loader's single-quoted export lines.
// extraDrop are names the calling runtime pins read-only before the load.
func envDataLoad(envFile string, extraDrop ...string) string {
	return envDataLoadWith(envFile, EnvDataControlLines(), extraDrop)
}

// envDataLoadWith is envDataLoad with the control lines given, so tests can
// point the .env.d and iteration.env paths at a temporary workspace.
func envDataLoadWith(envFile string, control []string, extraDrop []string) string {
	drop := append(append([]string{}, envDataDropNames...), extraDrop...)
	args := []string{shellQuote(envFile)}
	for _, line := range control {
		args = append(args, shellQuote(line))
	}
	args = append(args, shellQuote(strings.Join(drop, " ")))
	return envDataPin() + " && " + envDataExportsVar + `="$("$` + envDataPythonVar + `" -I -S -c ` +
		shellQuote(envDataLoaderScript) + " " + strings.Join(args, " ") + `)" && eval "$` +
		envDataExportsVar + `" && unset ` + envDataExportsVar
}

// envDataLoaderScript is the python3 program envDataLoad runs. argv: the .env
// path, the four control lines, then the space-separated drop names. It
// prints `[export ]NAME='value'` lines on stdout and exits non-zero, printing
// nothing, when .env or a file it names (an .env.d file or iteration.env)
// exists but cannot be read. Indented with tabs: the launch command is
// checked for double spaces.
const envDataLoaderScript = `import os, re, sys
env_file, cap_line, envd_line, restore_line, iter_line, drop_arg = sys.argv[1:7]
drop = set(drop_arg.split())
envd_dir = os.path.dirname(envd_line.split()[3])
iter_file = iter_line.split()[3]
NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
values = dict(os.environ)
exported = set(os.environ)
readonly = set()
order = []
failed = []
def warn(path, line, why):
	sys.stderr.write("fullsend: ignored %r line %d: %s\n" % (path, line, why))
def taken(name):
	return name not in drop and name not in readonly
def assign(path, line, name, value):
	if not taken(name):
		warn(path, line, "%s is not taken from .env" % name)
		return
	values[name] = value
	if name not in order:
		order.append(name)
def expand(s, i):
	if s.startswith("{", i):
		m = NAME.match(s, i + 1)
		if not m or not s.startswith("}", m.end()):
			raise ValueError("unsupported parameter expansion")
		return values.get(m.group(), ""), m.end() + 1
	m = NAME.match(s, i)
	if not m:
		raise ValueError("unsupported use of $")
	return values.get(m.group(), ""), m.end()
def value(s, i):
	out = []
	start = i
	while i < len(s):
		c = s[i]
		if c in " \t\n":
			break
		if c == "~" and (i == start or s[i - 1] == ":"):
			if i + 1 < len(s) and s[i + 1] not in "/: \t\n":
				raise ValueError("unsupported ~user")
			out.append(values.get("HOME", ""))
			i += 1
		elif c == "'":
			j = s.find("'", i + 1)
			if j < 0:
				raise ValueError("unterminated quote")
			out.append(s[i + 1:j])
			i = j + 1
		elif c == "\"":
			i += 1
			while True:
				if i >= len(s):
					raise ValueError("unterminated quote")
				c = s[i]
				if c == "\"":
					i += 1
					break
				if c == "\\" and i + 1 < len(s) and s[i + 1] in "$\"\\\x60\n":
					if s[i + 1] != "\n":
						out.append(s[i + 1])
					i += 2
				elif c == "$":
					v, i = expand(s, i + 1)
					out.append(v)
				elif c == "\x60":
					raise ValueError("command substitution")
				else:
					out.append(c)
					i += 1
		elif c == "$":
			v, i = expand(s, i + 1)
			out.append(v)
		elif c == "\\":
			if i + 1 >= len(s) or s[i + 1] == "\n":
				raise ValueError("line continuation")
			out.append(s[i + 1])
			i += 2
		elif c in ";&|<>()\x60":
			raise ValueError("not a plain assignment")
		else:
			out.append(c)
			i += 1
	return "".join(out), i
def end_of_statement(s, i):
	while i < len(s) and s[i] in " \t":
		i += 1
	if i < len(s) and s[i] == "#":
		i = s.find("\n", i)
		i = len(s) if i < 0 else i
	if i < len(s) and s[i] != "\n":
		raise ValueError("trailing text after the assignment")
	return i
def statement(path, line, s, i, eol):
	j = i
	export = False
	if s.startswith("export", j) and j + 6 < len(s) and s[j + 6] in " \t":
		export = True
		j += 6
		while j < len(s) and s[j] in " \t":
			j += 1
	m = NAME.match(s, j)
	if not m:
		raise ValueError("not a NAME=value assignment")
	name = m.group()
	j = m.end()
	if j < len(s) and s[j] == "=":
		v, j = value(s, j + 1)
		j = end_of_statement(s, j)
		assign(path, line, name, v)
		if export and taken(name):
			exported.add(name)
		return j
	if not export:
		raise ValueError("not a NAME=value assignment")
	names = s[i:eol].split()[1:]
	if not all(NAME.fullmatch(n) for n in names):
		raise ValueError("not a NAME=value assignment")
	for n in names:
		if taken(n):
			exported.add(n)
			if n in values and n not in order:
				order.append(n)
	return eol
def read_file(path, control):
	try:
		with open(path, encoding=sys.getfilesystemencoding(), errors="surrogateescape") as f:
			s = f.read()
	except OSError as e:
		return e
	i = 0
	while i < len(s):
		while i < len(s) and s[i] in " \t\n":
			i += 1
		if i >= len(s):
			break
		line = s.count("\n", 0, i) + 1
		eol = s.find("\n", i)
		eol = len(s) if eol < 0 else eol
		text = s[i:eol].rstrip(" \t")
		if text.startswith("#"):
			pass
		elif control and text == cap_line:
			if "FULLSEND_RUNNER_PATH" not in readonly:
				assign(path, line, "FULLSEND_RUNNER_PATH", values.get("PATH", ""))
				readonly.add("FULLSEND_RUNNER_PATH")
		elif control and text == envd_line:
			try:
				names = sorted(os.listdir(envd_dir))
			except OSError:
				names = []
			for n in names:
				p = os.path.join(envd_dir, n)
				if n.endswith(".env") and not n.startswith(".") and os.path.isfile(p):
					e = read_file(p, False)
					if e is not None:
						failed.append((p, e))
		elif control and text == restore_line:
			now = values.get("PATH", "")
			assign(path, line, "PATH", values.get("FULLSEND_RUNNER_PATH", "") + (":" + now if now else ""))
			exported.add("PATH")
		elif control and text == iter_line:
			if os.path.isfile(iter_file):
				e = read_file(iter_file, False)
				if e is not None:
					failed.append((iter_file, e))
		else:
			try:
				eol = statement(path, line, s, i, eol)
			except ValueError as e:
				warn(path, line, str(e))
		i = eol
	return None
err = read_file(env_file, True)
if err is not None:
	failed.insert(0, (env_file, err))
for path, e in failed:
	sys.stderr.write("fullsend: cannot read %r: %s\n" % (path, e.strerror))
if failed:
	sys.exit(1)
for n in order:
	if n in values and "\0" not in values[n]:
		sys.stdout.buffer.write(os.fsencode("%s%s='%s'\n" % ("export " if n in exported else "", n, values[n].replace("'", "'\\''"))))
`
