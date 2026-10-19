package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// getList fetches a list endpoint, accepting the {data, next_cursor} envelope or a bare array.
func getList[T any](c *client, p string, q url.Values) ([]T, *string, []byte, error) {
	raw, err := c.do("GET", p, q, nil, nil, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []T
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, nil, nil, fmt.Errorf("unexpected response from API: %v", err)
		}
		return items, nil, raw, nil
	}
	var env listEnvelope[T]
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, nil, nil, fmt.Errorf("unexpected response from API: %v", err)
		}
	}
	return env.Data, env.NextCursor, raw, nil
}

// --- login / logout / whoami / balance -----------------------------------

type deviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceToken struct {
	APIKey string `json:"api_key"`
	KeyID  string `json:"key_id"`
	User   struct {
		Email string `json:"email"`
	} `json:"user"`
}

func cmdLogin(a *app, args []string) error {
	fs := a.flagSet("login")
	key := fs.String("key", "", "API key to store instead of the browser flow")
	noBrowser := fs.Bool("no-browser", false, "print the code and URL without opening a browser")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	explicitURL := a.g.apiURL != ""

	if *key != "" {
		k := strings.TrimSpace(*key)
		c := a.client(false)
		c.key = k
		var me User
		if _, err := c.do("GET", "/me", nil, nil, nil, &me); err != nil {
			return err
		}
		if err := a.saveConfig(k, explicitURL); err != nil {
			return err
		}
		a.apiKey = k
		fmt.Fprintf(a.stdout, a.tr("Signed in as %s.\n", "Вы вошли как %s.\n"), me.Email)
		fmt.Fprintf(a.stdout, a.tr("API key saved to %s\n", "Ключ сохранён в %s\n"), a.configPath)
		return nil
	}

	tok, err := a.deviceLogin(*noBrowser)
	if err != nil {
		return err
	}
	if err := a.saveConfig(tok.APIKey, explicitURL); err != nil {
		return err
	}
	a.apiKey = tok.APIKey
	fmt.Fprintf(a.stdout, a.tr("\nSigned in as %s.\n", "\nВы вошли как %s.\n"), tok.User.Email)
	fmt.Fprintf(a.stdout, a.tr("API key saved to %s\n", "Ключ сохранён в %s\n"), a.configPath)
	return nil
}

func clientName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "cli"
	}
	if len(host) > 60 {
		host = host[:60]
	}
	return host
}

// deviceLogin runs the RFC 8628 style flow: show a code, poll until approved.
func (a *app) deviceLogin(noBrowser bool) (deviceToken, error) {
	c := a.client(false)
	var dc deviceCode
	if _, err := c.do("POST", "/auth/device", nil, map[string]string{"client_name": clientName()}, nil, &dc); err != nil {
		return deviceToken{}, err
	}
	if dc.DeviceCode == "" {
		return deviceToken{}, errors.New(a.tr("the server did not return a login code", "сервер не вернул код входа"))
	}
	fmt.Fprintf(a.stdout, a.tr("To sign in, open %s and enter this code:", "Чтобы войти, откройте %s и введите код:"), dc.VerificationURI)
	fmt.Fprintf(a.stdout, "\n\n    %s\n\n", a.p.bold(dc.UserCode))
	if !noBrowser {
		target := dc.VerificationURIComplete
		if target == "" {
			target = dc.VerificationURI
		}
		if err := a.openURL(target); err == nil {
			fmt.Fprintln(a.stdout, a.tr("Opened the sign-in page in your browser.", "Страница входа открыта в браузере."))
		}
	}

	interval := time.Duration(maxInt(dc.Interval, 1)) * time.Second
	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	deadline := a.now().Add(time.Duration(expiresIn) * time.Second)
	fmt.Fprintln(a.stdout, a.tr("Waiting for approval...", "Ждём подтверждения..."))

	expired := func() error {
		return &codeErr{code: exitAuth, err: errors.New(a.tr(
			"the code expired before it was approved; run `gpunz login` again",
			"код истёк до подтверждения; выполните `gpunz login` ещё раз"))}
	}
	for {
		if !a.now().Before(deadline) {
			return deviceToken{}, expired()
		}
		a.sleep(interval)
		var tok deviceToken
		_, err := c.do("POST", "/auth/device/token", nil, map[string]string{"device_code": dc.DeviceCode}, nil, &tok)
		if err == nil {
			if tok.APIKey == "" {
				return deviceToken{}, errors.New(a.tr("the server returned no API key", "сервер не вернул ключ"))
			}
			return tok, nil
		}
		var ae *APIError
		if !errors.As(err, &ae) {
			return deviceToken{}, err
		}
		switch {
		case ae.Code == "authorization_pending":
		case ae.Code == "slow_down" || ae.Status == 429:
			interval += 5 * time.Second
		case ae.Code == "access_denied":
			return deviceToken{}, &codeErr{code: exitAuth, err: errors.New(a.tr("sign-in was denied in the browser", "вход отклонён в браузере"))}
		case ae.Code == "expired_token":
			return deviceToken{}, expired()
		default:
			return deviceToken{}, err
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func cmdLogout(a *app, args []string) error {
	if a.fc.APIKey == "" {
		fmt.Fprintln(a.stdout, a.tr("No saved API key.", "Сохранённого ключа нет."))
	} else {
		fc := a.fc
		fc.APIKey = ""
		if err := writeConfig(a.configPath, fc); err != nil {
			return err
		}
		a.fc = fc
		fmt.Fprintln(a.stdout, a.tr("Signed out. The key stays active until you revoke it in the console.",
			"Вы вышли. Ключ останется активным, пока вы не отзовёте его в консоли."))
	}
	if a.getenv("GPUNZ_API_KEY") != "" {
		fmt.Fprintln(a.stderr, a.tr("warning: GPUNZ_API_KEY is still set in this environment", "внимание: GPUNZ_API_KEY всё ещё задан в окружении"))
	}
	return nil
}

func cmdWhoami(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("whoami"), args); err != nil {
		return err
	}
	var me User
	raw, err := a.client(true).do("GET", "/me", nil, nil, nil, &me)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	yes := a.tr("yes", "да")
	no := a.tr("no", "нет")
	tg := no
	if me.TelegramLinked {
		tg = yes
	}
	tfa := no
	if me.TwoFactor {
		tfa = yes
	}
	kv(a.stdout, a.p, [][2]string{
		{a.tr("Email", "E-mail"), me.Email},
		{a.tr("Name", "Имя"), strOr(me.Name, "—")},
		{a.tr("Two-factor", "Двухфакторная защита"), tfa},
		{a.tr("Telegram", "Telegram"), tg},
	})
	return nil
}

func cmdBalance(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("balance"), args); err != nil {
		return err
	}
	var b Balance
	raw, err := a.client(true).do("GET", "/balance", nil, nil, nil, &b)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	kv(a.stdout, a.p, [][2]string{
		{a.tr("Balance", "Баланс"), a.money(b.BalanceKop, b.Currency)},
		{a.tr("Available", "Доступно"), a.money(b.AvailableKop, b.Currency)},
		{a.tr("Credit limit", "Кредитный лимит"), a.money(b.CreditLimitKop, b.Currency)},
		{a.tr("Burn rate", "Расход"), a.rate(b.BurnKopH, b.Currency)},
		{a.tr("Runway", "Запас времени"), a.runway(b.RunwayHours)},
	})
	return nil
}

// accountCurrency is the balance currency of the signed-in account, read with one GET /balance. Anonymous callers and
// a failed read count as rubles, so the catalog never shows dollars it cannot back up.
func (a *app) accountCurrency() string {
	if a.apiKey == "" {
		return curRUB
	}
	var b Balance
	if _, err := a.client(true).do("GET", "/balance", nil, nil, nil, &b); err != nil || b.Currency != curUSD {
		return curRUB
	}
	return curUSD
}

// --- API keys -----------------------------------------------------------

var allScopes = []string{"user:read", "user:write", "instances:read", "instances:write", "billing:read"}

func cmdAPIKeys(a *app, args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return apiKeysList(a, args)
	case "create", "new":
		return apiKeysCreate(a, args)
	case "rm", "delete", "revoke":
		return apiKeysRemove(a, args)
	}
	return usageErr("%s: api-keys [ls|create|rm]", a.tr("unknown subcommand", "неизвестная подкоманда"))
}

func apiKeysList(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("api-keys ls"), args); err != nil {
		return err
	}
	items, _, raw, err := getList[ApiKey](a.client(true), "/api-keys", nil)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	t := table{head: []string{"ID", a.tr("NAME", "НАЗВАНИЕ"), "PREFIX", a.tr("SOURCE", "ИСТОЧНИК"), a.tr("LAST USED", "ИСПОЛЬЗОВАН"), a.tr("EXPIRES", "ИСТЕКАЕТ")}}
	for _, k := range items {
		t.add(k.ID, k.Name, k.Prefix+"…", k.Source, a.lastUsed(k.LastUsedAt), strOr(k.ExpiresAt, "—"))
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No API keys.", "API-ключей нет."))
		return nil
	}
	t.render(a.stdout, a.p)
	return nil
}

func (a *app) lastUsed(s *string) string {
	if s == nil || *s == "" {
		return a.tr("never", "никогда")
	}
	return timeLocal(*s)
}

func apiKeysCreate(a *app, args []string) error {
	fs := a.flagSet("api-keys create")
	name := fs.String("name", "", "key name (required)")
	scopes := fs.String("scopes", strings.Join(allScopes, ","), "comma-separated scopes")
	expires := fs.Int("expires-in-days", 0, "expire after N days (0 = never)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return usageErr("%s", a.tr("--name is required", "нужен --name"))
	}
	body := map[string]any{"name": strings.TrimSpace(*name), "scopes": splitList(*scopes)}
	if *expires > 0 {
		body["expires_in_days"] = *expires
	}
	var created ApiKeyCreated
	raw, err := a.client(true).do("POST", "/api-keys", nil, body, nil, &created)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	fmt.Fprintf(a.stdout, a.tr("Created API key %s (%s).\n", "Создан API-ключ %s (%s).\n"), created.Name, created.ID)
	fmt.Fprintf(a.stdout, a.tr("Secret (shown only once):\n%s\n", "Секрет (показывается один раз):\n%s\n"), a.p.bold(created.Secret))
	return nil
}

func apiKeysRemove(a *app, args []string) error {
	pos, err := parseArgs(a.flagSet("api-keys rm"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("%s", a.tr("usage: gpunz api-keys rm ID", "использование: gpunz api-keys rm ID"))
	}
	if _, err := a.client(true).do("DELETE", "/api-keys/"+url.PathEscape(pos[0]), nil, nil, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, a.tr("Deleted API key %s.\n", "API-ключ %s удалён.\n"), pos[0])
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- SSH keys -----------------------------------------------------------

func cmdSSHKeys(a *app, args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return sshKeysList(a, args)
	case "add":
		return sshKeysAdd(a, args)
	case "rm", "delete", "remove":
		return sshKeysRemove(a, args)
	}
	return usageErr("%s: ssh-keys [ls|add|rm]", a.tr("unknown subcommand", "неизвестная подкоманда"))
}

func sshKeysList(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("ssh-keys ls"), args); err != nil {
		return err
	}
	items, _, raw, err := getList[SshKey](a.client(true), "/ssh-keys", nil)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No SSH keys. Add one with: gpunz ssh-keys add", "SSH-ключей нет. Добавьте: gpunz ssh-keys add"))
		return nil
	}
	t := table{head: []string{"ID", a.tr("NAME", "НАЗВАНИЕ"), "TYPE", "FINGERPRINT", a.tr("ADDED", "ДОБАВЛЕН")}}
	for _, k := range items {
		t.add(k.ID, k.Name, k.Type, k.Fingerprint, timeLocal(k.CreatedAt))
	}
	t.render(a.stdout, a.p)
	return nil
}

func sshKeysAdd(a *app, args []string) error {
	fs := a.flagSet("ssh-keys add")
	name := fs.String("name", "", "key name (default: the key's comment)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	path := ""
	if len(pos) > 0 {
		path = pos[0]
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = filepath.Join(home, ".ssh", "id_ed25519.pub")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return &codeErr{code: exitGeneric, err: fmt.Errorf("%s: %v (%s)", path,
			err, a.tr("create one with: ssh-keygen -t ed25519", "создайте: ssh-keygen -t ed25519"))}
	}
	pub := strings.TrimSpace(string(data))
	if pub == "" {
		return usageErr("%s: %s", path, a.tr("the file is empty", "файл пуст"))
	}
	keyName := strings.TrimSpace(*name)
	if keyName == "" {
		if f := strings.Fields(pub); len(f) >= 3 {
			keyName = f[2]
		} else {
			keyName = clientName()
		}
	}
	var k SshKey
	raw, err := a.client(true).do("POST", "/ssh-keys", nil, map[string]string{"name": keyName, "public_key": pub}, nil, &k)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	fmt.Fprintf(a.stdout, a.tr("Added SSH key %s (%s) %s\n", "Добавлен SSH-ключ %s (%s) %s\n"), k.ID, k.Name, k.Fingerprint)
	return nil
}

func sshKeysRemove(a *app, args []string) error {
	pos, err := parseArgs(a.flagSet("ssh-keys rm"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("%s", a.tr("usage: gpunz ssh-keys rm ID", "использование: gpunz ssh-keys rm ID"))
	}
	if _, err := a.client(true).do("DELETE", "/ssh-keys/"+url.PathEscape(pos[0]), nil, nil, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, a.tr("Deleted SSH key %s.\n", "SSH-ключ %s удалён.\n"), pos[0])
	return nil
}
