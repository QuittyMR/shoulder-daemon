package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// The daemon injects text into a live coding session, and anything that can
// reach 127.0.0.1 can post to it — including a page open in a browser, for
// which localhost is not special. The shared secret in the hook header is what
// separates the harness from everything else on the machine.
//
// It is generated here rather than asked for because a setup step nobody
// performs is a daemon running with no authentication, and every install that
// is not a git checkout had exactly that. The value is kept in one file and
// copied into the places a harness reads its environment from, so the person
// who installed a plugin never learns that any of this happened.
const tokenBytes = 32

// envAssignment matches one line of the daemon's env file, which is the shape
// syncEnvFile has to rewrite without disturbing the rest of somebody's file.
var envAssignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Z][A-Z0-9_]*)\s*=\s*(.*)$`)

// ensureToken resolves the shared secret and reports whether the daemon had to
// invent it. An operator who set SHOULDER_TOKEN owns the value and nothing here
// touches their files.
func ensureToken(log *slog.Logger) (string, bool) {
	if tok := os.Getenv("SHOULDER_TOKEN"); tok != "" {
		return tok, false
	}

	path := tokenPath()
	if path == "" {
		return "", false
	}
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: our own state directory
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			// Written before, but the harness may have been installed since,
			// or its configuration replaced. Syncing on every start is what
			// makes this self-repairing rather than a one-time setup.
			syncToken(log, tok)
			return tok, true
		}
	}

	// A checkout install generates the value in its setup script and writes it
	// to the env file before any daemon has run. Adopting that rather than
	// generating a second one is what keeps the two from overwriting each
	// other's token every time either of them runs.
	if tok := config.Setting("SHOULDER_TOKEN"); tok != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			_ = os.WriteFile(path, []byte(tok+"\n"), 0o600)
		}
		syncToken(log, tok)
		return tok, true
	}

	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		log.Warn("could not generate a token; the daemon will accept any caller on the loopback interface",
			"error", err)
		return "", false
	}
	tok := hex.EncodeToString(buf)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warn("could not keep the generated token; it will be different after a restart",
			"path", path, "error", err)
		return tok, true
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		log.Warn("could not keep the generated token; it will be different after a restart",
			"path", path, "error", err)
	}
	syncToken(log, tok)
	return tok, true
}

// tokenPath is the daemon's own state, beside the store, not in the config
// directory: nobody types this value and nobody should have to.
func tokenPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "shoulder-daemon", "token")
}

// syncToken copies the value into every place a harness or a terminal reads it
// from. All of it is best-effort: a harness that is not installed is not a
// fault, and a daemon must not refuse to run because somebody's editor
// configuration is read-only.
func syncToken(log *slog.Logger, tok string) {
	if path := envFilePath(); path != "" {
		if err := syncEnvFile(path, tok); err != nil {
			log.Debug("could not write the token to the env file", "path", path, "error", err)
		}
	}
	for _, path := range claudeSettingsPaths() {
		switch err := syncClaudeSettings(path, tok); {
		case err == nil:
			log.Info("token written to the harness configuration", "path", path)
		case errors.Is(err, os.ErrNotExist):
			// Claude Code is not installed here, or has never been started.
		default:
			log.Warn("could not write the token to the harness configuration; hooks will be rejected until it holds the same value",
				"path", path, "error", err)
		}
	}
}

// claudeSettingsPaths is where Claude Code keeps the environment it
// interpolates hook headers from.
func claudeSettingsPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "settings.json")}
}

// syncEnvFile sets SHOULDER_TOKEN in the daemon's env file, leaving every other
// line as it was. The OpenCode adapter and the CLI both read this file, so a
// terminal that has sourced nothing can still talk to the daemon.
func syncEnvFile(path, tok string) error {
	line := fmt.Sprintf("SHOULDER_TOKEN=%q", tok)

	raw, err := os.ReadFile(path) //nolint:gosec // G304: SHOULDER_ENV_FILE is the operator's own setting
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	replaced := false
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if m := envAssignment.FindStringSubmatch(l); m != nil && m[1] == "SHOULDER_TOKEN" {
			if replaced {
				continue
			}
			out = append(out, line)
			replaced = true
			continue
		}
		if l != "" || len(out) > 0 {
			out = append(out, l)
		}
	}
	if !replaced {
		out = append(out, line)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600) //nolint:gosec // G703: SHOULDER_ENV_FILE is the operator's own setting
}

// syncClaudeSettings sets env.SHOULDER_TOKEN in Claude Code's settings, and
// changes nothing else. The file belongs to the person using it, so the order
// of their keys and the shape of every value they set is preserved rather than
// re-serialised into whatever this program would have written.
func syncClaudeSettings(path, tok string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the harness's own settings file
	if err != nil {
		return err
	}
	top, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	var env jsonObject
	if cur, ok := top.get("env"); ok {
		if env, err = decodeObject(cur); err != nil {
			return fmt.Errorf("%s: env: %w", path, err)
		}
	}
	want, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	if cur, ok := env.get("SHOULDER_TOKEN"); ok && bytes.Equal(bytes.TrimSpace(cur), want) {
		return nil
	}
	env.set("SHOULDER_TOKEN", want)

	body, err := env.encode("  ")
	if err != nil {
		return err
	}
	top.set("env", body)
	body, err = top.encode("")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(body, '\n'), 0o600)
}

// writeFileAtomic replaces a file through a temporary one beside it, so an
// interrupted write leaves the harness's settings intact rather than truncated.
func writeFileAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".shoulder-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// jsonObject is a JSON object with its keys in the order the file had them.
// encoding/json alone cannot do this: decoding into a map loses the order and
// re-encoding sorts it, which turns a one-value edit of somebody's settings
// into a rewrite of the whole file.
type jsonObject []jsonField

type jsonField struct {
	Key   string
	Value json.RawMessage
}

func (o jsonObject) get(key string) (json.RawMessage, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

func (o *jsonObject) set(key string, value json.RawMessage) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = value
			return
		}
	}
	*o = append(*o, jsonField{Key: key, Value: value})
}

func decodeObject(raw []byte) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out jsonObject
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		out = append(out, jsonField{Key: key, Value: value})
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}

// encode renders the object indented by two spaces, nested under prefix.
func (o jsonObject) encode(prefix string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, f := range o {
		key, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		buf.WriteString(prefix + "  ")
		buf.Write(key)
		buf.WriteString(": ")
		var value bytes.Buffer
		if err := json.Indent(&value, f.Value, prefix+"  ", "  "); err != nil {
			return nil, err
		}
		buf.Write(bytes.TrimSpace(value.Bytes()))
		if i < len(o)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString(prefix + "}")
	return buf.Bytes(), nil
}
