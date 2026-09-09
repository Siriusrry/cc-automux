package main

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// inspect-service reads the registration formats emitted by the installer.
// It never evaluates shell text or invokes a service manager.
func runInspectService(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: cc-automux inspect-service SERVICE_FILE")
		return 2
	}
	app, cfg, state, err := inspectService(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "inspect-service: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n%s\n%s\n", app, cfg, state)
	return 0
}

func inspectService(path string) (app, cfg, state string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	var binary string
	env := map[string]string{}
	if strings.HasSuffix(path, ".plist") {
		decoder := xml.NewDecoder(strings.NewReader(string(data)))
		var last string
		var inArgs bool
		for {
			token, tokenErr := decoder.Token()
			if tokenErr == io.EOF {
				break
			}
			if tokenErr != nil {
				return "", "", "", tokenErr
			}
			switch node := token.(type) {
			case xml.StartElement:
				switch node.Name.Local {
				case "key":
					if err := decoder.DecodeElement(&last, &node); err != nil {
						return "", "", "", err
					}
				case "array":
					inArgs = last == "ProgramArguments"
				case "string":
					var value string
					if err := decoder.DecodeElement(&value, &node); err != nil {
						return "", "", "", err
					}
					if inArgs && binary == "" {
						binary = value
					} else if !inArgs {
						env[last] = value
					}
				}
			case xml.EndElement:
				if node.Name.Local == "array" {
					inArgs = false
				}
			}
		}
	} else {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		section := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "[") {
				section = line
				continue
			}
			if section != "[Service]" {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "ExecStart":
				binary, err = unitValue(value)
				binary = strings.ReplaceAll(binary, "$$", "$")
			case "Environment":
				var assignment string
				assignment, err = unitValue(value)
				name, val, ok := strings.Cut(assignment, "=")
				if ok {
					env[name] = val
				}
			}
			if err != nil {
				return "", "", "", err
			}
		}
		if err := scanner.Err(); err != nil {
			return "", "", "", err
		}
	}
	app = filepath.Dir(filepath.Dir(binary))
	if !filepath.IsAbs(binary) || filepath.Base(binary) != "cc-automux" || filepath.Base(filepath.Dir(binary)) != "bin" || filepath.Base(app) != "cc-automux" {
		return "", "", "", errors.New("unrecognized service binary location; preserve the registration and resolve it manually")
	}
	cfg = env["CC_AUTOMUX_CONFIG"]
	if cfg == "" {
		cfg = filepath.Join(app, "config.json")
	}
	state = env["XDG_STATE_HOME"]
	if state == "" {
		state = os.Getenv("XDG_STATE_HOME")
	}
	if state == "" {
		state = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	for _, value := range []string{app, cfg, state} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\x00") {
			return "", "", "", errors.New("service paths must be absolute single-line values")
		}
	}
	return app, cfg, state, nil
}

func unitValue(value string) (string, error) {
	var err error
	if strings.HasPrefix(value, "\"") {
		value, err = strconv.Unquote(value)
	} else if strings.ContainsAny(value, " \t\"'\\") {
		err = errors.New("unsupported service directive quoting")
	}
	if err != nil {
		return "", err
	}
	// Only the literal percent escape is emitted by the installer. Never guess
	// the meaning of arbitrary systemd specifiers or environment expansion.
	if strings.Contains(strings.ReplaceAll(value, "%%", ""), "%") {
		return "", errors.New("unsupported service path specifier")
	}
	return strings.ReplaceAll(value, "%%", "%"), nil
}
