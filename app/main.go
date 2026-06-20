package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("$ ")

		input, err := reader.ReadString('\n')
		line := strings.TrimRight(input, "\r\n")

		if line != "" {
			fields := strings.Fields(line)
			name, args := fields[0], fields[1:]

			switch name {
			case "echo":
				fmt.Println(strings.Join(args, " "))
			case "exit":
				code := 0
				if len(args) > 0 {
					if parsed, perr := strconv.Atoi(args[0]); perr == nil {
						code = parsed
					}
				}
				os.Exit(code)
			default:
				fmt.Printf("%s: command not found\n", name)
			}
		}

		if err != nil { // io.EOF (Ctrl+D) or a read error: leave the REPL
			break
		}
	}
}
