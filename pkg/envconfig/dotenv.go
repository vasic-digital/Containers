package envconfig

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv reads a .env file and sets environment variables.
// Existing environment variables take precedence.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := parseDotEnvLine(line)
		if !ok {
			continue
		}

		// Only set if not already defined.
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

// LoadDotEnv is the exported version of loadDotEnv.
func LoadDotEnv(path string) error {
	return loadDotEnv(path)
}

// parseDotEnvLine parses a KEY=VALUE line, handling quotes and
// inline comments.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	// Remove export prefix.
	line = strings.TrimPrefix(line, "export ")

	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", "", false
	}

	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])

	// A quoted value may carry a trailing inline comment
	// (PASSWORD="s3cr3t" # note). Cut that comment BEFORE the
	// quote-stripping below inspects the value, so the closing quote is
	// once again the last character. Without this, the both-ends-quoted
	// strip is skipped (the last char is a comment char) and the value
	// keeps its literal surrounding quotes — corrupting an SSH password /
	// key into a wrong credential (ENV-4 / §11.4.10 quoted-path sibling);
	// worse, a comment that itself ends in a quote fools the both-ends
	// test into firing and embeds the comment text into the credential.
	// The '#' inside the quotes (e.g. "a#b") is untouched: we only look at
	// what follows the closing quote.
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		if rel := strings.IndexByte(value[1:], value[0]); rel >= 0 {
			rest := value[1+rel+1:]
			if t := strings.TrimLeft(rest, " \t"); t != rest &&
				strings.HasPrefix(t, "#") {
				value = value[:1+rel+1]
			}
		}
	}

	// Handle quoted values.
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
			return key, value, true
		}
	}

	// Strip inline comments (unquoted values only). A '#' is an
	// inline comment ONLY when it starts the value or is preceded by
	// whitespace (the dotenv convention). This preserves a '#' that is
	// part of an unquoted value — e.g. a password like p@ss#word —
	// instead of silently truncating it to p@ss (ENV-4 / §11.4.10
	// credential-corruption).
	for i := 0; i < len(value); i++ {
		if value[i] != '#' {
			continue
		}
		if i == 0 || value[i-1] == ' ' || value[i-1] == '\t' {
			value = strings.TrimSpace(value[:i])
			break
		}
	}

	return key, value, true
}
