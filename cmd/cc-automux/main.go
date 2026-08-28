package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/app"
	"github.com/Siriusrry/cc-automux/internal/config"
	productversion "github.com/Siriusrry/cc-automux/internal/version"
)

func main() {
	handled, exitCode := handleArgs(os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}
	app.Main()
}

func handleArgs(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, productversion.Display())
		return true, 0
	}
	if len(args) > 0 && args[0] == "init" {
		return true, runInit(args[1:], os.Stdin, stdout, stderr)
	}
	_, _ = fmt.Fprintln(stderr, "Usage: cc-automux [--version]")
	return true, 2
}

func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var path string
	generate := false
	service := config.ServiceConfig{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--generate-management-key":
			generate = true
		case "--config":
			if i+1 >= len(args) || args[i+1] == "" {
				_, _ = fmt.Fprintln(stderr, "init: --config requires an absolute path")
				return 2
			}
			path = args[i+1]
			i++
		case "--listen-addr":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				_, _ = fmt.Fprintln(stderr, "init: --listen-addr requires a value")
				return 2
			}
			service.ListenAddr = args[i+1]
			i++
		case "--log-max-bytes":
			if i+1 >= len(args) {
				_, _ = fmt.Fprintln(stderr, "init: --log-max-bytes requires a positive integer")
				return 2
			}
			value, parseErr := strconv.ParseInt(args[i+1], 10, 64)
			if parseErr != nil || value <= 0 {
				_, _ = fmt.Fprintln(stderr, "init: --log-max-bytes requires a positive integer")
				return 2
			}
			service.LogMaxBytes = value
			i++
		case "-h", "--help":
			_, _ = fmt.Fprintln(stdout, "Usage: cc-automux init [--generate-management-key] [--config PATH] [--listen-addr ADDR] [--log-max-bytes N]")
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "init: unknown argument %q\n", args[i])
			return 2
		}
	}
	if path == "" {
		var err error
		path, err = config.Path()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
			return 1
		}
	}

	// Existing configuration is inspected before prompting, so reinstalling or
	// rerunning init can never replace its management key.
	store, err := config.NewStore(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return 1
	}
	if _, err := store.Load(); err == nil {
		_, _ = fmt.Fprintf(stdout, "Configuration already exists: %s\n", path)
		return 0
	} else if !errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stderr, "init: inspect existing configuration: %v\n", err)
		return 1
	}

	managementKey := ""
	if generate {
		managementKey, err = config.GenerateManagementKey()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "Generated management key: %s\n", managementKey)
	} else {
		_, _ = fmt.Fprint(stdout, "Management key (input is visible): ")
		line, readErr := bufio.NewReader(stdin).ReadString('\n')
		if readErr != nil && len(line) == 0 {
			_, _ = fmt.Fprintf(stderr, "init: read management key: %v\n", readErr)
			return 1
		}
		managementKey = strings.TrimRight(line, "\r\n")
	}
	if _, created, err := config.InitializeWithService(path, managementKey, service); err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return 1
	} else if created {
		_, _ = fmt.Fprintf(stdout, "Initialized configuration: %s\n", path)
	} else {
		_, _ = fmt.Fprintf(stdout, "Configuration already exists: %s\n", path)
	}
	return 0
}
