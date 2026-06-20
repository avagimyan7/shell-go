package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

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

func main() {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("$ ")

		input, err := reader.ReadString('\n')
		fields := tokenize(strings.TrimRight(input, "\r\n"))

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
