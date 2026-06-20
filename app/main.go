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

	"golang.org/x/term"
)

const prompt = "$ "

var builtins = map[string]bool{
	"cd":   true,
	"echo": true,
	"exit": true,
	"pwd":  true,
	"type": true,
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
func candidatesFor(line string) (head, word string, matches []string) {
	if i := strings.LastIndex(line, " "); i >= 0 {
		head, word = line[:i+1], line[i+1:]
		return head, word, fileCandidates(word)
	}
	return "", line, completionCandidates(line)
}

// fileCandidates returns the sorted names of entries in the current directory
// that start with prefix.
func fileCandidates(prefix string) []string {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
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
			head, word, matches := candidatesFor(string(buf))
			switch {
			case len(matches) == 0:
				fmt.Print("\a") // bell: nothing to complete
				lastTab = false
			case len(matches) == 1:
				completed := matches[0] + " "
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
					fmt.Print("\r\n" + strings.Join(matches, "  ") + "\r\n" + prompt + string(buf))
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
		fmt.Print(prompt)

		line, err := readLine(in, fd, interactive)
		fields := tokenize(line)

		if len(fields) > 0 {
			if words, stdout, stderr, cleanup, ok := applyRedirections(fields); ok {
				if len(words) > 0 {
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
