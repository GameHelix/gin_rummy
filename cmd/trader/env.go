package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loadEnv handles simple dotenv assignments without evaluating shell code,
// reading them the way Docker Compose does: an unquoted value ends at " #".
// Duplicate keys are rejected so local runs and the container never differ.
// Existing process variables always win. It never prints values.
func loadEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !envKeyPattern.MatchString(key) {
			return fmt.Errorf("invalid dotenv assignment on line %d", i+1)
		}
		if seen[key] {
			return fmt.Errorf("%s is set twice in %s", key, path)
		}
		seen[key] = true
		if value != "" && (value[0] == '\'' || value[0] == '"') {
			end := strings.IndexByte(value[1:], value[0])
			if end < 0 {
				return fmt.Errorf("unterminated quote on line %d", i+1)
			}
			if rest := strings.TrimSpace(value[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return fmt.Errorf("invalid dotenv assignment on line %d", i+1)
			}
			value = value[1 : end+1]
		} else if cut := strings.Index(value, " #"); cut >= 0 {
			value = strings.TrimSpace(value[:cut])
		} else if strings.HasPrefix(value, "#") {
			value = ""
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}
