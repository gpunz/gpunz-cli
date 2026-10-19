// Command gpunz is the command-line client for gpu.nz.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev"

const (
	exitOK       = 0
	exitGeneric  = 1
	exitUsage    = 2
	exitAuth     = 3
	exitNotFound = 4
	exitFunds    = 5
)

const (
	defaultAPIBase    = "https://gpu.nz/api/v1"
	defaultUpdateBase = "https://gpu.nz/cli/latest/"
)

var errHelp = errors.New("help requested")

// codeErr attaches an explicit process exit status to an error.
type codeErr struct {
	code int
	err  error
}

func (e *codeErr) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *codeErr) Unwrap() error { return e.err }

func usageErr(format string, args ...any) error {
	return &codeErr{code: exitUsage, err: fmt.Errorf(format, args...)}
}

type globals struct {
	json   bool
	apiURL string
	apiKey string
}

// app holds the process environment, so tests can drive it with fakes.
type app struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	getenv     func(string) string
	configPath string
	cfgErr     error
	fc         fileConfig
	g          globals
	apiBase    string
	apiKey     string
	ru         bool
	p          paint
	stdinTTY   bool
	hc         *http.Client
	sleep      func(time.Duration)
	now        func() time.Time
	openURL    func(string) error
	updateBase string
	exePath    func() (string, error)
}

func newApp(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) *app {
	a := &app{
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		getenv:     getenv,
		hc:         &http.Client{Timeout: 90 * time.Second},
		sleep:      time.Sleep,
		now:        time.Now,
		openURL:    openBrowser,
		updateBase: defaultUpdateBase,
		exePath:    os.Executable,
	}
	if base := getenv("GPUNZ_UPDATE_URL"); base != "" {
		a.updateBase = base
	}
	if dir, err := configDir(); err == nil {
		a.configPath = filepath.Join(dir, "config.json")
	} else {
		a.cfgErr = err
	}
	a.ru = detectRu(getenv, systemLocale())
	if f, ok := stdout.(*os.File); ok && isTerminal(f) && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb" {
		a.p.on = enableVT(f)
	}
	if f, ok := stdin.(*os.File); ok {
		a.stdinTTY = isTerminal(f)
	}
	return a
}

// tr picks the Russian or English string.
func (a *app) tr(en, ru string) string {
	if a.ru {
		return ru
	}
	return en
}

func detectRu(getenv func(string) string, sysLocale string) bool {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			return strings.HasPrefix(strings.ToLower(v), "ru")
		}
	}
	return strings.HasPrefix(strings.ToLower(sysLocale), "ru")
}

// parseGlobals removes global flags wherever they appear before "--".
func parseGlobals(argv []string) (globals, []string, error) {
	var g globals
	var rest []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(arg, "=")
		switch name {
		case "--json":
			g.json = true
			continue
		case "--api-url", "--api-key":
			if !hasVal {
				if i+1 >= len(argv) {
					return g, nil, usageErr("%s needs a value", name)
				}
				i++
				val = argv[i]
			}
			if name == "--api-url" {
				g.apiURL = val
			} else {
				g.apiKey = val
			}
			continue
		}
		rest = append(rest, arg)
	}
	return g, rest, nil
}

// loadConfig reads the config file. A broken file is remembered, not fatal, so
// commands that need no config still work.
func (a *app) loadConfig() {
	if a.configPath == "" {
		return
	}
	fc, err := readConfig(a.configPath)
	if err != nil {
		a.cfgErr = err
		return
	}
	a.fc = fc
}

func (a *app) resolveConn() {
	a.apiBase = normalizeBase(firstNonEmpty(a.g.apiURL, a.getenv("GPUNZ_API_URL"), a.fc.APIURL, defaultAPIBase))
	a.apiKey = firstNonEmpty(a.g.apiKey, a.getenv("GPUNZ_API_KEY"), a.fc.APIKey)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizeBase turns "https://gpu.nz" into "https://gpu.nz/api/v1".
func normalizeBase(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasSuffix(s, "/api/v1") {
		s += "/api/v1"
	}
	return s
}

func (a *app) client(withKey bool) *client {
	key := ""
	if withKey {
		key = a.apiKey
	}
	lang := "en"
	if a.ru {
		lang = "ru"
	}
	return &client{base: a.apiBase, key: key, hc: a.hc, lang: lang, ua: "gpunz/" + version}
}

// command is one CLI verb.
type command struct {
	name     string
	aliases  []string
	en, ru   string
	usage    string
	needAuth bool
	run      func(a *app, args []string) error
}

func (a *app) commands() []command {
	return []command{
		{name: "login", en: "Sign in (browser device flow or --key)", ru: "Вход (через браузер или --key)", usage: "login [--key KEY] [--no-browser]", run: cmdLogin},
		{name: "logout", en: "Forget the saved API key", ru: "Удалить сохранённый ключ", usage: "logout", run: cmdLogout},
		{name: "whoami", en: "Show the signed-in account", ru: "Показать аккаунт", usage: "whoami", needAuth: true, run: cmdWhoami},
		{name: "balance", en: "Show balance and runway", ru: "Баланс и запас времени", usage: "balance", needAuth: true, run: cmdBalance},
		{name: "topup", en: "Top up the balance (opens the payment page, shows a QR code)", ru: "Пополнить баланс (страница оплаты и QR-код)", usage: "topup [AMOUNT] [--no-browser] [--no-qr]", run: cmdTopup},
		{name: "gpus", en: "List GPU models", ru: "Список моделей GPU", usage: "gpus", run: cmdGpus},
		{name: "search", en: "Search rentable offers", ru: "Поиск предложений", usage: "search [filters]", run: cmdSearch},
		{name: "templates", en: "List launch templates", ru: "Шаблоны запуска", usage: "templates", run: cmdTemplates},
		{name: "ssh-keys", en: "Manage SSH keys (ls|add|rm)", ru: "SSH-ключи (ls|add|rm)", usage: "ssh-keys [ls|add [PATH]|rm ID]", needAuth: true, run: cmdSSHKeys},
		{name: "create", en: "Rent an offer", ru: "Арендовать предложение", usage: "create OFFER_ID --template SLUG [--disk GB] [flags]", needAuth: true, run: cmdCreate},
		{name: "instances", aliases: []string{"ls"}, en: "List instances", ru: "Список серверов", usage: "instances [--all]", needAuth: true, run: cmdInstances},
		{name: "show", en: "Show one instance", ru: "Детали сервера", usage: "show ID", needAuth: true, run: cmdShow},
		{name: "start", en: "Start a stopped instance", ru: "Запустить сервер", usage: "start ID", needAuth: true, run: cmdStart},
		{name: "stop", en: "Stop an instance (keeps disk)", ru: "Остановить сервер (диск сохраняется)", usage: "stop ID", needAuth: true, run: cmdStop},
		{name: "destroy", en: "Destroy an instance", ru: "Удалить сервер", usage: "destroy ID [-y]", needAuth: true, run: cmdDestroy},
		{name: "ssh", en: "Open an SSH session", ru: "SSH-сессия", usage: "ssh ID [ssh args...]", needAuth: true, run: cmdSSH},
		{name: "logs", en: "Show instance logs", ru: "Логи сервера", usage: "logs ID [--tail N]", needAuth: true, run: cmdLogs},
		{name: "api-keys", en: "Manage API keys (ls|create|rm)", ru: "API-ключи (ls|create|rm)", usage: "api-keys [ls|create --name N|rm ID]", needAuth: true, run: cmdAPIKeys},
		{name: "version", en: "Print the CLI version", ru: "Версия CLI", usage: "version", run: cmdVersion},
		{name: "update", en: "Update the CLI to the latest release", ru: "Обновить CLI до последней версии", usage: "update [--force]", run: cmdUpdate},
		{name: "completion", en: "Print shell completion (bash|zsh|fish|powershell)", ru: "Скрипт автодополнения", usage: "completion bash|zsh|fish|powershell", run: cmdCompletion},
	}
}

func (a *app) findCommand(name string) *command {
	cmds := a.commands()
	for i := range cmds {
		if cmds[i].name == name {
			return &cmds[i]
		}
		for _, al := range cmds[i].aliases {
			if al == name {
				return &cmds[i]
			}
		}
	}
	return nil
}

func (a *app) printUsage() {
	w := a.stdout
	fmt.Fprintln(w, a.tr("gpunz: command-line client for gpu.nz GPU cloud", "gpunz: командная строка для GPU-облака gpu.nz"))
	fmt.Fprintln(w)
	fmt.Fprintln(w, a.tr("Usage:", "Использование:")+" gpunz <command> [flags]")
	fmt.Fprintln(w)
	for _, c := range a.commands() {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, a.tr(c.en, c.ru))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, a.tr("Global flags: --json  --api-url URL  --api-key KEY", "Глобальные флаги: --json  --api-url URL  --api-key KEY"))
	fmt.Fprintln(w, a.tr("Run 'gpunz <command> --help' for command flags.", "Справка по команде: gpunz <команда> --help"))
}

func (a *app) printCommandHelp(c *command) {
	fmt.Fprintf(a.stdout, "%s\n\n", a.tr(c.en, c.ru))
	fmt.Fprintf(a.stdout, "%s gpunz %s\n", a.tr("Usage:", "Использование:"), c.usage)
	if len(c.aliases) > 0 {
		fmt.Fprintf(a.stdout, "%s %s\n", a.tr("Aliases:", "Синонимы:"), strings.Join(c.aliases, ", "))
	}
}

// run executes argv and returns the process exit status.
func (a *app) run(argv []string) int {
	g, rest, err := parseGlobals(argv)
	if err != nil {
		return a.fail(err)
	}
	a.g = g
	a.loadConfig()
	a.resolveConn()
	if a.cfgErr != nil {
		fmt.Fprintf(a.stderr, "warning: %v\n", a.cfgErr)
	}
	if len(rest) == 0 {
		a.printUsage()
		return exitOK
	}
	switch rest[0] {
	case "-h", "--help", "help":
		if len(rest) > 1 {
			if c := a.findCommand(rest[1]); c != nil {
				a.printCommandHelp(c)
				return exitOK
			}
		}
		a.printUsage()
		return exitOK
	case "--version", "-v":
		return a.finish(cmdVersion(a, nil))
	}
	c := a.findCommand(rest[0])
	if c == nil {
		return a.fail(usageErr("%s: %s (gpunz help)", a.tr("unknown command", "неизвестная команда"), rest[0]))
	}
	args := rest[1:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		a.printCommandHelp(c)
		return exitOK
	}
	if c.needAuth && a.apiKey == "" {
		return a.fail(a.notLoggedIn())
	}
	return a.finish(c.run(a, args))
}

func (a *app) finish(err error) int {
	if err == nil {
		return exitOK
	}
	return a.fail(err)
}

func (a *app) notLoggedIn() error {
	return &codeErr{code: exitAuth, err: errors.New(a.tr(
		"not signed in: run `gpunz login` or set GPUNZ_API_KEY",
		"вы не вошли: выполните `gpunz login` или задайте GPUNZ_API_KEY"))}
}

// fail prints an error and returns its exit status.
func (a *app) fail(err error) int {
	if errors.Is(err, errHelp) {
		return exitOK
	}
	code := exitCode(err)
	var ae *APIError
	if errors.As(err, &ae) {
		if a.g.json {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": ae.Code, "message": ae.Message, "details": ae.Details}})
			fmt.Fprintln(a.stderr, string(b))
		} else {
			msg := ae.Message
			if ae.Code == "rate_limited" && ae.RetryAfter != "" {
				msg += a.tr(" (retry after ", " (повторите через ") + ae.RetryAfter + a.tr("s)", " с)")
			}
			fmt.Fprintf(a.stderr, "%s %s %s\n", a.p.red("error:"), msg, a.p.dim("("+ae.Code+")"))
		}
		return code
	}
	if msg := err.Error(); msg != "" {
		fmt.Fprintf(a.stderr, "%s %s\n", a.p.red("error:"), msg)
	}
	return code
}

func exitCode(err error) int {
	var ce *codeErr
	if errors.As(err, &ce) {
		return ce.code
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Code == "insufficient_funds" || ae.Status == 402:
			return exitFunds
		case ae.Status == 401 || ae.Status == 403:
			return exitAuth
		case ae.Status == 404:
			return exitNotFound
		}
	}
	return exitGeneric
}

// flagSet builds a FlagSet that never prints on its own; parseArgs reports errors.
func (a *app) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseArgs parses flags that may come before, between or after positionals.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, usageErr("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// writeRaw prints a raw API response as indented JSON (for --json).
func (a *app) writeRaw(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	_, err := a.stdout.Write(buf.Bytes())
	return err
}

// config file -------------------------------------------------------------

type fileConfig struct {
	APIKey string `json:"api_key,omitempty"`
	APIURL string `json:"api_url,omitempty"`
}

func readConfig(path string) (fileConfig, error) {
	var fc fileConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fc, nil
	}
	if err != nil {
		return fc, err
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		return fileConfig{}, fmt.Errorf("%s: invalid JSON: %v", path, err)
	}
	return fc, nil
}

func writeConfig(path string, fc fileConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp already makes the file 0600 on Unix; Windows ignores modes.
	return os.Rename(tmp.Name(), path)
}

// saveConfig writes the API key (and URL when given explicitly) to the config file.
func (a *app) saveConfig(key string, explicitURL bool) error {
	fc := a.fc
	fc.APIKey = key
	if explicitURL {
		fc.APIURL = a.apiBase
	}
	if err := writeConfig(a.configPath, fc); err != nil {
		return err
	}
	a.fc = fc
	return nil
}

func main() {
	a := newApp(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	os.Exit(a.run(os.Args[1:]))
}
