package main

import (
	"fmt"
	"strconv"
	"strings"
)

// flagScanner parses a subcommand's arguments in ONE pass, so flags may appear
// before OR after positional arguments: `yanshi session show <id> -json` and
// `yanshi session show -json <id>` both work.
//
// It exists because the stdlib flag.FlagSet stops at the first non-flag
// argument, and that failure is silent in the worst way: the trailing flag
// lands in Args() as a positional, so the command prints usage and exits 2 for
// a spelling that looks correct. Measured twice — `yanshi session show <id>
// -json` and `yanshi ipc initialize -root DIR` — which is why this is shared
// rather than written per command.
//
// A flag may carry its value attached (-config=x) or separate (-config x).
// A bare "-" is a positional (stdin convention). An unknown flag is an ERROR:
// a silently dropped -json is the difference between a script parsing output
// and the same script failing on text it cannot read.
type flagScanner struct {
	bools  map[string]bool
	values map[string]string
	seen   map[string]string
	pos    []string
}

// newFlagScanner builds a scanner for one command's flag vocabulary.
func newFlagScanner(boolFlags, valueFlags []string) *flagScanner {
	s := &flagScanner{
		bools:  make(map[string]bool, len(boolFlags)),
		values: make(map[string]string, len(valueFlags)),
		seen:   map[string]string{},
	}
	for _, f := range boolFlags {
		s.bools[f] = true
	}
	for _, f := range valueFlags {
		s.values[f] = ""
	}
	return s
}

// parse consumes args and records positionals.
func (s *flagScanner) parse(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			s.pos = append(s.pos, a)
			continue
		}
		name, val, attached := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case s.bools[name]:
			if attached {
				return fmt.Errorf("flag -%s takes no value", name)
			}
			s.seen[name] = "true"
		case name == "help" || name == "h":
			return errHelpRequested
		default:
			if _, ok := s.values[name]; !ok {
				return fmt.Errorf("unknown flag -%s", name)
			}
			if !attached {
				if i+1 >= len(args) {
					return fmt.Errorf("flag -%s needs a value", name)
				}
				i++
				val = args[i]
			}
			s.seen[name] = val
		}
	}
	return nil
}

// errHelpRequested is returned by parse for -h/-help so callers can print their
// usage and exit 0 (help is not a usage error).
var errHelpRequested = fmt.Errorf("help requested")

// has reports whether a boolean flag was given.
func (s *flagScanner) has(name string) bool { _, ok := s.seen[name]; return ok }

// value returns a value flag's argument, or def when it was not given.
func (s *flagScanner) value(name, def string) string {
	if v, ok := s.seen[name]; ok {
		return v
	}
	return def
}

// intValue parses an integer value flag.
func (s *flagScanner) intValue(name string, def int) (int, error) {
	v, ok := s.seen[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("flag -%s needs an integer, got %q", name, v)
	}
	return n, nil
}

// positionals returns the non-flag arguments in order.
func (s *flagScanner) positionals() []string { return s.pos }
