package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// newIdempotencyKey returns a random key; one process run reuses the same key on retries.
func newIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "gpunz-" + strconv.FormatInt(int64(os.Getpid()), 10)
	}
	return hex.EncodeToString(b)
}

func cmdCreate(a *app, args []string) error {
	fs := a.flagSet("create")
	tmpl := fs.String("template", "", "template slug (required, see: gpunz templates)")
	disk := fs.Int("disk", 0, "disk size, GB (default: the template's recommended size)")
	name := fs.String("name", "", "instance name")
	typ := fs.String("type", "on_demand", "on_demand or interruptible")
	onstart := fs.String("onstart", "", "shell script run on start")
	var sshKeys, envs multiFlag
	fs.Var(&sshKeys, "ssh-key", "SSH key ID to attach (repeatable; default: all your keys)")
	fs.Var(&envs, "env", "environment variable KEY=VALUE (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("%s", a.tr("usage: gpunz create OFFER_ID --template SLUG [--disk GB]",
			"использование: gpunz create OFFER_ID --template SLUG [--disk GB]"))
	}
	offerID := pos[0]
	if *tmpl == "" {
		return usageErr("%s", a.tr("--template is required (see: gpunz templates)", "нужен --template (см. gpunz templates)"))
	}
	if *typ != "on_demand" && *typ != "interruptible" {
		return usageErr("%s", a.tr("--type must be on_demand or interruptible", "--type должен быть on_demand или interruptible"))
	}
	c := a.client(true)
	if *disk < 0 {
		return usageErr("%s", a.tr("--disk must be positive", "--disk должен быть больше нуля"))
	}
	if *disk == 0 {
		// Use the template's recommended disk size when --disk is omitted.
		items, _, _, err := getList[Template](c, "/templates", nil)
		if err != nil {
			return err
		}
		for _, t := range items {
			if t.Slug == *tmpl && t.RecommendedDiskGB > 0 {
				*disk = t.RecommendedDiskGB
			}
		}
		if *disk == 0 {
			return usageErr("%s", a.tr("--disk is required (GB)", "нужен --disk (ГБ)"))
		}
	}
	body := map[string]any{
		"offer_id": offerID,
		"template": *tmpl,
		"disk_gb":  *disk,
		"type":     *typ,
	}
	if *name != "" {
		body["name"] = *name
	}
	if len(sshKeys) > 0 {
		body["ssh_key_ids"] = []string(sshKeys)
	}
	if *onstart != "" {
		body["onstart"] = *onstart
	}
	if len(envs) > 0 {
		env := map[string]string{}
		for _, kv := range envs {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return usageErr("%s: %s", a.tr("--env expects KEY=VALUE", "--env ожидает KEY=VALUE"), kv)
			}
			env[k] = v
		}
		body["env"] = env
	}
	var inst Instance
	raw, err := c.do("POST", "/instances", nil, body, map[string]string{"Idempotency-Key": newIdempotencyKey()}, &inst)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	fmt.Fprintf(a.stdout, a.tr("Instance %s is %s. Details: gpunz show %s\n", "Сервер %s: %s. Подробнее: gpunz show %s\n"),
		inst.ID, a.statusStyle(inst.Status), inst.ID)
	return nil
}

func cmdInstances(a *app, args []string) error {
	fs := a.flagSet("instances")
	all := fs.Bool("all", false, "include destroyed instances")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	if *all {
		q.Set("status", "all")
	}
	items, _, raw, err := getList[Instance](a.client(true), "/instances", q)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No instances. Find an offer with: gpunz search", "Серверов нет. Поиск предложений: gpunz search"))
		return nil
	}
	t := table{head: []string{"ID", a.tr("NAME", "НАЗВАНИЕ"), a.tr("STATUS", "СТАТУС"), "GPU", a.tr("TYPE", "ТИП"),
		a.tr("TEMPLATE", "ШАБЛОН"), a.tr("RATE", "ЦЕНА"), a.tr("COST", "ИТОГО"), a.tr("CREATED", "СОЗДАН")}}
	for _, in := range items {
		t.add(in.ID, strOr(in.Name, "—"), a.statusStyle(in.Status), fmt.Sprintf("%s ×%d", in.GPU.Name, in.GPU.Count),
			typeLabel(a, in.Type), in.Template.Slug, a.rate(in.Price.RunningKopH, in.Currency), a.money(in.CostSoFarKop, in.Currency), timeLocal(in.CreatedAt))
	}
	t.render(a.stdout, a.p)
	return nil
}

func typeLabel(a *app, typ string) string {
	if typ == "interruptible" {
		return a.tr("interruptible", "прерываемый")
	}
	return a.tr("on-demand", "по требованию")
}

// instanceID reads one positional instance ID; flags are parsed into fs.
func instanceID(a *app, fs *flag.FlagSet, args []string, usage string) (string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", usageErr("%s: gpunz %s", a.tr("usage", "использование"), usage)
	}
	return pos[0], nil
}

func cmdShow(a *app, args []string) error {
	id, err := instanceID(a, a.flagSet("show"), args, "show ID")
	if err != nil {
		return err
	}
	var in Instance
	raw, err := a.client(true).do("GET", "/instances/"+url.PathEscape(id), nil, nil, nil, &in)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	pairs := [][2]string{
		{"ID", in.ID},
		{a.tr("Name", "Название"), strOr(in.Name, "—")},
		{a.tr("Status", "Статус"), a.statusStyle(in.Status)},
		{a.tr("Type", "Тип"), typeLabel(a, in.Type)},
		{"GPU", fmt.Sprintf("%s ×%d, %d GB VRAM", in.GPU.Name, in.GPU.Count, in.GPU.VRAMGB)},
		{a.tr("Template", "Шаблон"), in.Template.Slug},
		{a.tr("Disk", "Диск"), fmt.Sprintf("%d GB", in.DiskGB)},
		{a.tr("Country", "Страна"), strings.ToUpper(in.Country)},
		{a.tr("Rate", "Цена"), a.rate(in.Price.RunningKopH, in.Currency) + a.tr(" running, ", " в работе, ") + a.rate(in.Price.StoppedKopH, in.Currency) + a.tr(" stopped", " остановлен")},
		{a.tr("Cost so far", "Итого"), a.money(in.CostSoFarKop, in.Currency)},
		{a.tr("Created", "Создан"), timeLocal(in.CreatedAt) + " (" + a.ago(in.CreatedAt) + ")"},
	}
	if in.StartedAt != nil && *in.StartedAt != "" {
		pairs = append(pairs, [2]string{a.tr("Started", "Запущен"), timeLocal(*in.StartedAt)})
	}
	if in.StopReason != nil && *in.StopReason != "" {
		pairs = append(pairs, [2]string{a.tr("Stop reason", "Причина остановки"), *in.StopReason})
	}
	if in.SSH != nil {
		pairs = append(pairs, [2]string{"SSH", in.SSH.Command})
	}
	for _, u := range in.URLs {
		pairs = append(pairs, [2]string{u.Label, u.URL})
	}
	kv(a.stdout, a.p, pairs)
	return nil
}

func cmdStart(a *app, args []string) error { return instanceAction(a, args, "start") }
func cmdStop(a *app, args []string) error  { return instanceAction(a, args, "stop") }

// instanceAction handles start and stop.
func instanceAction(a *app, args []string, verb string) error {
	fs := a.flagSet(verb)
	accept := fs.Bool("accept-price", false, "start at the re-quoted price when it changed")
	id, err := instanceID(a, fs, args, verb+" ID [--accept-price]")
	if err != nil {
		return err
	}
	body := map[string]any{}
	if *accept {
		body["accept_price"] = true
	}
	var in Instance
	raw, err := a.client(true).do("POST", "/instances/"+url.PathEscape(id)+"/"+verb, nil, body, nil, &in)
	if err != nil {
		var ae *APIError
		if verb == "start" && errors.As(err, &ae) && ae.Code == "price_changed" {
			// the server was re-quoted at the market price: show old → new and ask for the confirmation
			// the amounts are in details.currency (absent on older servers: rubles)
			var d struct {
				CurrentKopH float64 `json:"current_kop_h"`
				NewKopH     float64 `json:"new_kop_h"`
				Currency    string  `json:"currency"`
			}
			_ = json.Unmarshal(ae.Details, &d)
			fmt.Fprintf(a.stdout, a.tr("The price changed: %s → %s. Start at the new price with: gpunz start %s --accept-price\n",
				"Цена изменилась: %s → %s. Запустить по новой цене: gpunz start %s --accept-price\n"),
				a.rate(int64(math.Round(d.CurrentKopH)), d.Currency), a.rate(int64(math.Round(d.NewKopH)), d.Currency), id)
		}
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	fmt.Fprintf(a.stdout, a.tr("Instance %s: %s\n", "Сервер %s: %s\n"), in.ID, a.statusStyle(in.Status))
	return nil
}

func cmdDestroy(a *app, args []string) error {
	fs := a.flagSet("destroy")
	yes := fs.Bool("y", false, "skip the confirmation prompt")
	fs.BoolVar(yes, "yes", false, "skip the confirmation prompt")
	id, err := instanceID(a, fs, args, "destroy ID [-y]")
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(a.stdout, a.tr("Destroy instance %s? Its disk is deleted and this cannot be undone. Type y to confirm: ",
			"Удалить сервер %s? Диск будет удалён, это необратимо. Введите y для подтверждения: "), id)
		line, _ := bufio.NewReader(a.stdin).ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		if ans != "y" && ans != "yes" && ans != "д" && ans != "да" {
			fmt.Fprintln(a.stdout, a.tr("Cancelled.", "Отменено."))
			return nil
		}
	}
	var in Instance
	raw, err := a.client(true).do("DELETE", "/instances/"+url.PathEscape(id), nil, nil, nil, &in)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	fmt.Fprintf(a.stdout, a.tr("Instance %s: %s\n", "Сервер %s: %s\n"), id, a.statusStyle(in.Status))
	return nil
}

func cmdSSH(a *app, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageErr("%s", a.tr("usage: gpunz ssh ID [ssh args...]", "использование: gpunz ssh ID [аргументы ssh]"))
	}
	id, extra := args[0], args[1:]
	var in Instance
	if _, err := a.client(true).do("GET", "/instances/"+url.PathEscape(id), nil, nil, nil, &in); err != nil {
		return err
	}
	if in.SSH == nil {
		return &codeErr{code: exitGeneric, err: fmt.Errorf(a.tr("instance %s has no SSH access yet (status: %s)",
			"у сервера %s пока нет SSH-доступа (статус: %s)"), id, in.Status)}
	}
	sshPath, err := findSSH()
	if err != nil {
		return &codeErr{code: exitGeneric, err: fmt.Errorf("%s: %s", a.tr("OpenSSH client not found; connect manually with", "клиент OpenSSH не найден; подключитесь вручную:"), in.SSH.Command)}
	}
	target := in.SSH.User + "@" + in.SSH.Host
	sshArgs := []string{"-p", strconv.Itoa(in.SSH.Port), target}
	sshArgs = append(sshArgs, extra...)
	cmd := exec.Command(sshPath, sshArgs...)
	cmd.Stdin = a.stdin
	cmd.Stdout = a.stdout
	cmd.Stderr = a.stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &codeErr{code: ee.ExitCode()}
		}
		return err
	}
	return nil
}

// findSSH locates the system ssh client; on Windows that is OpenSSH's ssh.exe.
func findSSH() (string, error) {
	if p, err := exec.LookPath("ssh"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		if root := os.Getenv("SystemRoot"); root != "" {
			p := filepath.Join(root, "System32", "OpenSSH", "ssh.exe")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("ssh not found")
}

func cmdLogs(a *app, args []string) error {
	fs := a.flagSet("logs")
	tail := fs.Int("tail", 200, "number of lines (1-1000)")
	id, err := instanceID(a, fs, args, "logs ID [--tail N]")
	if err != nil {
		return err
	}
	if *tail < 1 {
		*tail = 1
	}
	if *tail > 1000 {
		*tail = 1000
	}
	var out struct {
		Lines []string `json:"lines"`
	}
	raw, err := a.client(true).do("GET", "/instances/"+url.PathEscape(id)+"/logs",
		url.Values{"tail": {strconv.Itoa(*tail)}}, nil, nil, &out)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	for _, l := range out.Lines {
		fmt.Fprintln(a.stdout, l)
	}
	return nil
}
