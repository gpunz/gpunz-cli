package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func cmdVersion(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("version"), args); err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw([]byte(fmt.Sprintf(`{"version":%q,"os":%q,"arch":%q}`, version, runtime.GOOS, runtime.GOARCH)))
	}
	fmt.Fprintf(a.stdout, "gpunz %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	return nil
}

// binaryName is the release asset for this platform, e.g. gpunz-linux-amd64.
func binaryName(goos, goarch string) string {
	name := "gpunz-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func cmdUpdate(a *app, args []string) error {
	fs := a.flagSet("update")
	force := fs.Bool("force", false, "reinstall even when already up to date")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	exe, err := a.exePath()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	removeOldBinary(exe)

	latest, err := fetchText(a.hc, a.updateBase+"VERSION")
	if err != nil {
		return err
	}
	latest = strings.TrimSpace(latest)
	if latest == version && !*force {
		fmt.Fprintf(a.stdout, a.tr("gpunz %s is already the latest version.\n", "gpunz %s уже последней версии.\n"), version)
		return nil
	}
	fmt.Fprintf(a.stdout, a.tr("Downloading gpunz %s...\n", "Загружаем gpunz %s...\n"), latest)
	tmp, err := downloadVerified(a.hc, a.updateBase, runtime.GOOS, runtime.GOARCH, filepath.Dir(exe))
	if err != nil {
		return err
	}
	if err := replaceExecutable(exe, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %v", a.tr("cannot replace the binary; try again with administrator rights", "не удалось заменить файл; повторите с правами администратора"), err)
	}
	fmt.Fprintf(a.stdout, a.tr("Updated gpunz %s -> %s (%s)\n", "gpunz обновлён: %s -> %s (%s)\n"), version, latest, exe)
	return nil
}

func fetchText(hc *http.Client, u string) (string, error) {
	resp, err := hc.Get(u)
	if err != nil {
		return "", fmt.Errorf("cannot download %s: %v", u, unwrapURLErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// expectedSum finds the SHA-256 for name in a sha256sum-style listing.
func expectedSum(sums, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// downloadVerified fetches the binary for goos/goarch into dir and checks its SHA-256
// against SHA256SUMS. It returns the temp file path; the file is never installed unverified.
func downloadVerified(hc *http.Client, base, goos, goarch, dir string) (string, error) {
	name := binaryName(goos, goarch)
	sums, err := fetchText(hc, base+"SHA256SUMS")
	if err != nil {
		return "", err
	}
	want, ok := expectedSum(sums, name)
	if !ok {
		return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
	}
	resp, err := hc.Get(base + name)
	if err != nil {
		return "", fmt.Errorf("cannot download %s: %v", name, unwrapURLErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, "gpunz-update-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", name, want, got)
	}
	if goos != "windows" {
		if err := os.Chmod(tmp.Name(), 0o755); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
	}
	return tmp.Name(), nil
}

// replaceExecutable swaps exe for the verified tmp file. Windows cannot overwrite a
// running binary, but it can rename one, so the old file is moved aside first.
func replaceExecutable(exe, tmp string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(tmp, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	// the running binary is locked and stays as .old until the next run (removeOldBinary); a copy that is not running
	// goes now
	os.Remove(old)
	return nil
}

// removeOldBinary deletes the leftover from a previous Windows update, if it is unlocked.
func removeOldBinary(exe string) {
	os.Remove(exe + ".old")
}

var completionCommands = []string{"login", "logout", "whoami", "balance", "topup", "gpus", "search", "templates",
	"ssh-keys", "create", "instances", "ls", "show", "start", "stop", "destroy", "ssh", "logs", "api-keys",
	"version", "update", "completion"}

var completionFlags = map[string][]string{
	"login":     {"--key", "--no-browser"},
	"topup":     {"--no-browser", "--no-qr"},
	"search":    {"--gpu", "--num-gpus", "--type", "--max-price", "--country", "--min-reliability", "--verified", "--min-vram", "--min-cuda", "--min-disk", "--min-down", "--order", "--limit"},
	"create":    {"--template", "--disk", "--name", "--type", "--ssh-key", "--env", "--onstart"},
	"instances": {"--all"},
	"ls":        {"--all"},
	"destroy":   {"-y"},
	"logs":      {"--tail"},
	"api-keys":  {"ls", "create", "rm"},
	"ssh-keys":  {"ls", "add", "rm"},
	"update":    {"--force"},
}

var globalFlags = []string{"--json", "--api-url", "--api-key"}

func cmdCompletion(a *app, args []string) error {
	if len(args) != 1 {
		return usageErr("%s", a.tr("usage: gpunz completion bash|zsh|fish|powershell", "использование: gpunz completion bash|zsh|fish|powershell"))
	}
	cmds := strings.Join(completionCommands, " ")
	var flagLines strings.Builder
	for _, c := range completionCommands {
		if f, ok := completionFlags[c]; ok {
			fmt.Fprintf(&flagLines, "    %s) COMPREPLY=( $(compgen -W %q -- \"$cur\") ); return ;;\n", c, strings.Join(append(f, globalFlags...), " "))
		}
	}
	switch args[0] {
	case "bash", "zsh":
		prefix := ""
		if args[0] == "zsh" {
			prefix = "autoload -U +X bashcompinit && bashcompinit\n"
		}
		fmt.Fprintf(a.stdout, `%s_gpunz() {
  local cur="${COMP_WORDS[COMP_CWORD]}" cmd="${COMP_WORDS[1]}"
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=( $(compgen -W %q -- "$cur") ); return
  fi
  case "$cmd" in
%s    *) COMPREPLY=( $(compgen -W %q -- "$cur") ) ;;
  esac
}
complete -F _gpunz gpunz
`, prefix, cmds, flagLines.String(), strings.Join(globalFlags, " "))
	case "fish":
		fmt.Fprintf(a.stdout, "complete -c gpunz -f -n __fish_use_subcommand -a %q\n", cmds)
		for _, c := range completionCommands {
			for _, f := range completionFlags[c] {
				if strings.HasPrefix(f, "--") {
					fmt.Fprintf(a.stdout, "complete -c gpunz -f -n '__fish_seen_subcommand_from %s' -l %s\n", c, strings.TrimPrefix(f, "--"))
				}
			}
		}
		fmt.Fprintln(a.stdout, "complete -c gpunz -f -l json\ncomplete -c gpunz -f -l api-url\ncomplete -c gpunz -f -l api-key")
	case "powershell":
		quoted := make([]string, len(completionCommands))
		for i, c := range completionCommands {
			quoted[i] = "'" + c + "'"
		}
		fmt.Fprintf(a.stdout, `Register-ArgumentCompleter -Native -CommandName gpunz -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $cmds = @(%s)
    $cmds | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`, strings.Join(quoted, ", "))
	default:
		return usageErr("%s: %s", a.tr("unknown shell", "неизвестная оболочка"), args[0])
	}
	return nil
}
