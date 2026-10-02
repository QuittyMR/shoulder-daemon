package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

const envUsage = `usage: shoulderd env path | get NAME | set NAME [VALUE] | unset NAME

Read and write the daemon's env file with the grammar the daemon, the CLI and
compose read it with, so that what a script writes is what they all read.

  path        the file: $SHOULDER_ENV_FILE, or ~/.config/shoulder-daemon/env
  get NAME    the file's value for NAME, expanded; nothing when it sets none
  set NAME    write NAME, from VALUE or, without one, from standard input, so
              a secret never has to be an argument anybody can list
  unset NAME  remove every line that sets NAME
`

// stdin is where `env set` reads a value it was not given; tests replace it.
var stdin io.Reader = os.Stdin

func (c *cli) env(args []string) int {
	if len(args) == 0 {
		return c.reject(errors.New("env needs a verb: path, get, set or unset"))
	}
	path := config.EnvFilePath()
	switch verb, rest := args[0], args[1:]; verb {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(c.out, envUsage)
		return 0
	case "path":
		fmt.Fprintln(c.out, path)
		return 0
	case "get":
		if len(rest) != 1 {
			return c.reject(errors.New("usage: shoulderd env get NAME"))
		}
		if v := config.FileSetting(rest[0]); v != "" {
			fmt.Fprintln(c.out, v)
		}
		return 0
	case "set":
		if len(rest) != 1 && len(rest) != 2 {
			return c.reject(errors.New("usage: shoulderd env set NAME [VALUE]"))
		}
		var value string
		if len(rest) == 2 {
			value = rest[1]
		} else {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				fmt.Fprintln(c.err, "shoulderd: env set:", err)
				return 1
			}
			value = strings.TrimSuffix(string(raw), "\n")
		}
		if err := config.SetInFile(path, rest[0], &value); err != nil {
			fmt.Fprintln(c.err, "shoulderd: env set:", err)
			return 1
		}
		return 0
	case "unset":
		if len(rest) != 1 {
			return c.reject(errors.New("usage: shoulderd env unset NAME"))
		}
		if err := config.SetInFile(path, rest[0], nil); err != nil {
			fmt.Fprintln(c.err, "shoulderd: env unset:", err)
			return 1
		}
		return 0
	}
	return c.reject(fmt.Errorf("unknown env verb %q: use path, get, set or unset", args[0]))
}
