package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"
)

const prompt = "$ "

var builtins = map[string]bool{
	"cd":       true,
	"complete": true,
	"echo":     true,
	"exit":     true,
	"jobs":     true,
	"pwd":      true,
	"type":     true,
}

// completers maps a command name to the path of its registered `complete -C`
// completer script.
var completers = map[string]string{}

// job records a background command for the jobs builtin.
type job struct {
	num     int
	pid     int
	command string // the command as run, without the trailing &
}

var jobList []*job

// nextJobNum returns one more than the highest job number currently tracked
// (so numbers recycle to 1 once the table empties).
func nextJobNum() int {
	max := 0
	for _, j := range jobList {
		if j.num > max {
			max = j.num
		}
	}
	return max + 1
}

// jobMarker is "+" for the highest-numbered job, "-" for the second-highest,
// and a space for the rest.
func jobMarker(num int) string {
	first, second := 0, 0
	for _, j := range jobList {
		switch {
		case j.num > first:
			first, second = j.num, first
		case j.num > second:
			second = j.num
		}
	}
	switch num {
	case first:
		return "+"
	case second:
		return "-"
	default:
		return " "
	}
}

func formatJob(j *job, status string) string {
	command := j.command
	if status == "Running" {
		command += " &"
	}
	return fmt.Sprintf("[%d]%s  %-24s%s\n", j.num, jobMarker(j.num), status, command)
}

// exited non-blockingly checks whether the job's process has finished, reaping
// it in the process.
func (j *job) exited() bool {
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(j.pid, &ws, syscall.WNOHANG, nil)
	return err == nil && pid == j.pid
}

// reap reports finished background jobs as Done and drops them from the table.
// When showRunning is set (the jobs builtin) it also lists still-running jobs.
func reap(out *os.File, showRunning bool) {
	var remaining []*job
	var buf strings.Builder
	for _, j := range jobList {
		if j.exited() {
			buf.WriteString(formatJob(j, "Done"))
		} else {
			if showRunning {
				buf.WriteString(formatJob(j, "Running"))
			}
			remaining = append(remaining, j)
		}
	}
	jobList = remaining
	fmt.Fprint(out, buf.String())
}

// tokenize splits a command line into arguments following POSIX shell quoting
// rules: single quotes preserve everything literally, double quotes allow a
// backslash to escape $ ` " \ (and newline), and an unquoted backslash escapes
// the next character. Adjacent quoted/unquoted runs concatenate into one token.
func tokenize(line string) []string {
	const (
		normal = iota
		single
		double
	)

	var tokens []string
	var cur strings.Builder
	state, inToken := normal, false

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch state {
		case single:
			if c == '\'' {
				state = normal
			} else {
				cur.WriteByte(c)
			}
		case double:
			switch {
			case c == '"':
				state = normal
			case c == '\\' && i+1 < len(line) && isDoubleQuoteEscape(line[i+1]):
				i++
				cur.WriteByte(line[i])
			default:
				cur.WriteByte(c)
			}
		default: // normal
			switch {
			case c == '\'':
				state, inToken = single, true
			case c == '"':
				state, inToken = double, true
			case c == '\\' && i+1 < len(line):
				i++
				cur.WriteByte(line[i])
				inToken = true
			case c == ' ' || c == '\t':
				if inToken {
					tokens = append(tokens, cur.String())
					cur.Reset()
					inToken = false
				}
			default:
				cur.WriteByte(c)
				inToken = true
			}
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func isDoubleQuoteEscape(c byte) bool {
	return c == '$' || c == '`' || c == '"' || c == '\\' || c == '\n'
}

// applyRedirections scans tokens for redirection operators (>, 1>, 2>, and their
// >> append forms), opens the target files, and returns the remaining command
// words plus the stdout/stderr writers to use. Files are created/truncated (or
// appended) up front, mirroring how a real shell sets up redirections before
// running the command.
func applyRedirections(fields []string) (words []string, stdout, stderr *os.File, cleanup func(), ok bool) {
	stdout, stderr = os.Stdout, os.Stderr
	var opened []*os.File
	cleanup = func() {
		for _, f := range opened {
			f.Close()
		}
	}

	for i := 0; i < len(fields); i++ {
		var dst **os.File
		var mode int
		switch fields[i] {
		case ">", "1>":
			dst, mode = &stdout, os.O_TRUNC
		case ">>", "1>>":
			dst, mode = &stdout, os.O_APPEND
		case "2>":
			dst, mode = &stderr, os.O_TRUNC
		case "2>>":
			dst, mode = &stderr, os.O_APPEND
		default:
			words = append(words, fields[i])
			continue
		}

		if i+1 >= len(fields) {
			fmt.Fprintln(os.Stderr, "syntax error near unexpected token `newline'")
			cleanup()
			return nil, nil, nil, func() {}, false
		}
		i++
		f, ferr := os.OpenFile(fields[i], os.O_WRONLY|os.O_CREATE|mode, 0644)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", fields[i], ferr)
			cleanup()
			return nil, nil, nil, func() {}, false
		}
		opened = append(opened, f)
		*dst = f
	}
	return words, stdout, stderr, cleanup, true
}

func run(words []string, stdout, stderr *os.File) {
	name, args := words[0], words[1:]

	switch name {
	case "echo":
		fmt.Fprintln(stdout, strings.Join(args, " "))
	case "pwd":
		if dir, gerr := os.Getwd(); gerr == nil {
			fmt.Fprintln(stdout, dir)
		} else {
			fmt.Fprintln(stderr, "pwd:", gerr)
		}
	case "cd":
		dir := "~"
		if len(args) > 0 {
			dir = args[0]
		}
		target := dir
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			if home, herr := os.UserHomeDir(); herr == nil {
				target = home + dir[1:]
			}
		}
		if cerr := os.Chdir(target); cerr != nil {
			fmt.Fprintf(stderr, "cd: %s: No such file or directory\n", dir)
		}
	case "exit":
		code := 0
		if len(args) > 0 {
			if parsed, perr := strconv.Atoi(args[0]); perr == nil {
				code = parsed
			}
		}
		os.Exit(code)
	case "type":
		if len(args) > 0 {
			target := args[0]
			if builtins[target] {
				fmt.Fprintf(stdout, "%s is a shell builtin\n", target)
			} else if path, lerr := exec.LookPath(target); lerr == nil {
				fmt.Fprintf(stdout, "%s is %s\n", target, path)
			} else {
				fmt.Fprintf(stdout, "%s: not found\n", target)
			}
		}
	case "complete":
		completeBuiltin(args, stdout, stderr)
	case "jobs":
		reap(stdout, true)
	default:
		if _, lerr := exec.LookPath(name); lerr != nil {
			fmt.Fprintf(stderr, "%s: command not found\n", name)
		} else {
			cmd := exec.Command(name, args...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			cmd.Run() // exit status not tracked yet; child output is forwarded directly
		}
	}
}

// splitPipeline splits tokens on the `|` operator into per-command stages.
func splitPipeline(fields []string) [][]string {
	var stages [][]string
	var cur []string
	for _, f := range fields {
		if f == "|" {
			stages = append(stages, cur)
			cur = nil
		} else {
			cur = append(cur, f)
		}
	}
	return append(stages, cur)
}

func hasPipe(fields []string) bool {
	for _, f := range fields {
		if f == "|" {
			return true
		}
	}
	return false
}

func closePipeEnd(f *os.File) {
	if f != os.Stdin && f != os.Stdout && f != os.Stderr {
		f.Close()
	}
}

// runPipeline wires each stage's stdout to the next stage's stdin and runs them
// concurrently. Builtins run in goroutines (in this process); external commands
// are spawned. The first stage reads the terminal, the last writes to it.
func runPipeline(stages [][]string) {
	n := len(stages)
	ins := make([]*os.File, n)
	outs := make([]*os.File, n)
	ins[0], outs[n-1] = os.Stdin, os.Stdout
	for i := 0; i < n-1; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		outs[i], ins[i+1] = w, r
	}

	var wg sync.WaitGroup
	var cmds []*exec.Cmd
	for i, stage := range stages {
		if len(stage) == 0 {
			continue
		}
		in, out := ins[i], outs[i]
		if builtins[stage[0]] {
			wg.Add(1)
			go func(words []string, in, out *os.File) {
				defer wg.Done()
				run(words, out, os.Stderr)
				closePipeEnd(in)
				closePipeEnd(out)
			}(stage, in, out)
			continue
		}
		cmd := exec.Command(stage[0], stage[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: command not found\n", stage[0])
		} else {
			cmds = append(cmds, cmd)
		}
		closePipeEnd(in)
		closePipeEnd(out)
	}

	for _, cmd := range cmds {
		cmd.Wait()
	}
	wg.Wait()
}

// runBackground starts a command without waiting for it, letting it inherit the
// shell's stdio so its output still reaches the terminal, then prints the
// assigned job number and PID.
func runBackground(words []string, stdout, stderr *os.File) {
	if len(words) == 0 {
		return
	}
	name, args := words[0], words[1:]
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "%s: command not found\n", name)
		return
	}
	num := nextJobNum()
	jobList = append(jobList, &job{num: num, pid: cmd.Process.Pid, command: strings.Join(words, " ")})
	fmt.Printf("[%d] %d\n", num, cmd.Process.Pid)
}

// completeBuiltin implements the `complete` builtin: -C registers a completer
// script for one or more commands, -p prints a command's registered spec (or an
// error if none), and -r removes a command's spec.
func completeBuiltin(args []string, stdout, stderr *os.File) {
	if len(args) < 2 {
		return
	}
	switch args[0] {
	case "-C":
		for _, command := range args[2:] {
			completers[command] = args[1]
		}
	case "-p":
		command := args[1]
		if script, ok := completers[command]; ok {
			fmt.Fprintf(stdout, "complete -C '%s' %s\n", script, command)
		} else {
			fmt.Fprintf(stderr, "complete: %s: no completion specification\n", command)
		}
	case "-r":
		delete(completers, args[1])
	}
}

// completionCandidates returns the sorted, de-duplicated set of builtin and
// PATH-executable names that start with prefix.
func completionCandidates(prefix string) []string {
	set := map[string]struct{}{}

	for name := range builtins {
		if strings.HasPrefix(name, prefix) {
			set[name] = struct{}{}
		}
	}

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // PATH may list directories that don't exist or aren't readable
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, prefix) && isExecutable(filepath.Join(dir, name)) {
				set[name] = struct{}{}
			}
		}
	}

	matches := make([]string, 0, len(set))
	for name := range set {
		matches = append(matches, name)
	}
	sort.Strings(matches)
	return matches
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// candidatesFor splits the input line into the part before the word being
// completed (head, including the trailing space) and the partial word itself,
// then returns the completion candidates. The first word completes against
// commands (builtins + PATH); later words complete against files in the cwd.
func candidatesFor(line string) (head, word string, matches []string, isFile bool) {
	i := strings.LastIndex(line, " ")
	if i < 0 {
		// completing the command itself: builtins + PATH executables
		return "", line, completionCandidates(line), false
	}
	head, word = line[:i+1], line[i+1:]
	// completing an argument: a registered completer script wins over files
	if fields := strings.Fields(line); len(fields) > 0 {
		if script, ok := completers[fields[0]]; ok {
			prev := ""
			if hf := strings.Fields(head); len(hf) > 0 {
				prev = hf[len(hf)-1]
			}
			return head, word, runCompleter(script, fields[0], word, prev, line), false
		}
	}
	return head, word, fileCandidates(word), true
}

// runCompleter invokes a registered `complete -C` script, passing the command,
// the word being completed, and the previous word as arguments, plus COMP_LINE
// and COMP_POINT in the environment. Each non-empty stdout line is a candidate.
func runCompleter(script, command, word, prev, line string) []string {
	cmd := exec.Command(script, command, word, prev)
	cmd.Env = append(os.Environ(),
		"COMP_LINE="+line,
		fmt.Sprintf("COMP_POINT=%d", len(line)),
	)
	out, _ := cmd.Output()

	var cands []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			cands = append(cands, l)
		}
	}
	sort.Strings(cands)
	return cands
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// completionSuffix is appended after a fully completed token: a slash for a
// directory (so the user can keep descending the path) or a space otherwise.
func completionSuffix(match string, isFile bool) string {
	if isFile && isDir(match) {
		return "/"
	}
	return " "
}

// displayMatches renders the match list shown on a double Tab, marking
// directories with a trailing slash.
func displayMatches(matches []string, isFile bool) string {
	if !isFile {
		return strings.Join(matches, "  ")
	}
	shown := make([]string, len(matches))
	for i, m := range matches {
		if isDir(m) {
			shown[i] = m + "/"
		} else {
			shown[i] = m
		}
	}
	return strings.Join(shown, "  ")
}

// fileCandidates returns the sorted entries that match the file word being
// completed. If the word contains a slash, the part up to the last slash is the
// directory to list and the rest is the prefix; candidates keep that directory
// prefix so they replace the whole typed path.
func fileCandidates(word string) []string {
	dir, prefix := "", word
	if i := strings.LastIndex(word, "/"); i >= 0 {
		dir, prefix = word[:i+1], word[i+1:]
	}
	listPath := dir
	if listPath == "" {
		listPath = "."
	}
	entries, err := os.ReadDir(listPath)
	if err != nil {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			matches = append(matches, dir+name)
		}
	}
	sort.Strings(matches)
	return matches
}

func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}

// editLine reads one line in raw mode, echoing input itself and handling Tab
// completion, Backspace, Enter, and Ctrl-C/Ctrl-D.
func editLine(in *bufio.Reader) (string, error) {
	var buf []byte
	lastTab := false // the previous keypress was a Tab on this same input

	for {
		b, err := in.ReadByte()
		if err != nil {
			return string(buf), err
		}

		if b == '\t' {
			head, word, matches, isFile := candidatesFor(string(buf))
			switch {
			case len(matches) == 0:
				fmt.Print("\a") // bell: nothing to complete
				lastTab = false
			case len(matches) == 1:
				completed := matches[0] + completionSuffix(matches[0], isFile)
				fmt.Print(completed[len(word):])
				buf = []byte(head + completed)
				lastTab = false
			default:
				if lcp := longestCommonPrefix(matches); len(lcp) > len(word) {
					// extend to the longest common prefix (no trailing space yet)
					fmt.Print(lcp[len(word):])
					buf = []byte(head + lcp)
					lastTab = false
				} else if lastTab { // second Tab: list all matches, then redraw the prompt
					fmt.Print("\r\n" + displayMatches(matches, isFile) + "\r\n" + prompt + string(buf))
					lastTab = false
				} else { // first Tab: ring the bell
					fmt.Print("\a")
					lastTab = true
				}
			}
			continue
		}

		lastTab = false
		switch b {
		case '\r', '\n':
			fmt.Print("\r\n")
			return string(buf), nil
		case 0x7f, 0x08: // Backspace / Delete
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}
		case 0x03: // Ctrl-C: discard the line and start fresh
			fmt.Print("\r\n")
			return "", nil
		case 0x04: // Ctrl-D: end the shell only on an empty line
			if len(buf) == 0 {
				return "", io.EOF
			}
		default:
			if b >= 0x20 { // printable character (or a UTF-8 continuation byte)
				buf = append(buf, b)
				fmt.Print(string(b))
			}
		}
	}
}

// readLine reads one command line. On a real terminal it switches to raw mode
// (for Tab completion and key handling) just for the duration of the read, then
// restores cooked mode so command output is processed normally. When stdin is
// not a terminal (e.g. piped input) it reads a plain line.
func readLine(in *bufio.Reader, fd int, interactive bool) (string, error) {
	if interactive {
		if oldState, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, oldState)
			return editLine(in)
		}
	}
	s, err := in.ReadString('\n')
	return strings.TrimRight(s, "\r\n"), err
}

func main() {
	in := bufio.NewReader(os.Stdin)
	fd := int(os.Stdin.Fd())
	interactive := term.IsTerminal(fd)

	for {
		reap(os.Stdout, false) // report & remove finished background jobs before prompting

		fmt.Print(prompt)

		line, err := readLine(in, fd, interactive)
		fields := tokenize(line)

		if len(fields) > 0 {
			if hasPipe(fields) {
				runPipeline(splitPipeline(fields))
			} else if words, stdout, stderr, cleanup, ok := applyRedirections(fields); ok {
				if n := len(words); n > 0 && words[n-1] == "&" {
					runBackground(words[:n-1], stdout, stderr)
				} else if n > 0 {
					run(words, stdout, stderr)
				}
				cleanup()
			}
		}

		if err != nil { // io.EOF (Ctrl+D) or a read error: leave the REPL
			break
		}
	}
}
