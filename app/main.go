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

func main() {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("$ ")

		input, err := reader.ReadString('\n')
		fields := tokenize(strings.TrimRight(input, "\r\n"))

		if len(fields) > 0 {
			name, args := fields[0], fields[1:]

			switch name {
			case "echo":
				fmt.Println(strings.Join(args, " "))
			case "pwd":
				if dir, gerr := os.Getwd(); gerr == nil {
					fmt.Println(dir)
				} else {
					fmt.Fprintln(os.Stderr, "pwd:", gerr)
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
					fmt.Fprintf(os.Stderr, "cd: %s: No such file or directory\n", dir)
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
						fmt.Printf("%s is a shell builtin\n", target)
					} else if path, lerr := exec.LookPath(target); lerr == nil {
						fmt.Printf("%s is %s\n", target, path)
					} else {
						fmt.Printf("%s: not found\n", target)
					}
				}
			default:
				if _, lerr := exec.LookPath(name); lerr != nil {
					fmt.Printf("%s: command not found\n", name)
				} else {
					cmd := exec.Command(name, args...)
					cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
					cmd.Run() // exit status not tracked yet; child output is forwarded directly
				}
			}
		}

		if err != nil { // io.EOF (Ctrl+D) or a read error: leave the REPL
			break
		}
	}
}
