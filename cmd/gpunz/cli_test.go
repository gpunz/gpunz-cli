package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// harness drives the app with an in-memory environment and captured output.
type harness struct {
	t      *testing.T
	a      *app
	out    *bytes.Buffer
	errb   *bytes.Buffer
	opened []string
	cfg    string
}

func newHarness(t *testing.T, env map[string]string, stdin string) *harness {
	t.Helper()
	h := &harness{t: t, out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["LANG"]; !ok {
		env["LANG"] = "en_US.UTF-8"
	}
	getenv := func(k string) string { return env[k] }
	h.a = newApp(strings.NewReader(stdin), h.out, h.errb, getenv)
	h.cfg = filepath.Join(t.TempDir(), "gpunz", "config.json")
	h.a.configPath = h.cfg
	h.a.sleep = func(time.Duration) {}
	h.a.openURL = func(u string) error {
		h.opened = append(h.opened, u)
		return nil
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.errb.Reset()
	return h.a.run(args)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// fakeAPI serves the given routes under /api/v1 and returns the server.
func fakeAPI(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, fn := range routes {
		mux.HandleFunc(pattern, fn)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sampleOffer(id string, kop int64) map[string]any {
	return map[string]any{
		"id": id, "available": true, "verified": true, "country": "de",
		"gpu":         map[string]any{"slug": "rtx-4090", "name": "RTX 4090", "count": 2, "vram_gb": 24},
		"cpu":         map[string]any{"name": "EPYC", "cores": 32},
		"ram_gb":      128,
		"disk":        map[string]any{"max_gb": 1000},
		"network":     map[string]any{"up_mbps": 500, "down_mbps": 1000},
		"reliability": 0.992,
		"price":       map[string]any{"on_demand_kop_h": kop, "interruptible_kop_h": 1200, "storage_kop_gb_month": 1000, "traffic_kop_gb": 0},
	}
}

// --- config and argument parsing ------------------------------------------

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeConfig(path, fileConfig{APIKey: "gpz_file", APIURL: "https://file.example/api/v1"}); err != nil {
		t.Fatal(err)
	}
	fc, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		flagURL string
		flagKey string
		env     map[string]string
		wantURL string
		wantKey string
	}{
		{"file only", "", "", nil, "https://file.example/api/v1", "gpz_file"},
		{"env beats file", "", "", map[string]string{"GPUNZ_API_KEY": "gpz_env", "GPUNZ_API_URL": "http://env.example"}, "http://env.example/api/v1", "gpz_env"},
		{"flag beats env", "http://flag.example/", "gpz_flag", map[string]string{"GPUNZ_API_KEY": "gpz_env", "GPUNZ_API_URL": "http://env.example"}, "http://flag.example/api/v1", "gpz_flag"},
		{"default", "", "", map[string]string{"GPUNZ_API_KEY": ""}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newApp(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, func(k string) string { return tc.env[k] })
			a.fc = fc
			if tc.name == "default" {
				a.fc = fileConfig{}
			}
			a.g = globals{apiURL: tc.flagURL, apiKey: tc.flagKey}
			a.resolveConn()
			if tc.wantKey != "" && a.apiKey != tc.wantKey {
				t.Fatalf("key = %q, want %q", a.apiKey, tc.wantKey)
			}
			if tc.wantURL != "" && a.apiBase != tc.wantURL {
				t.Fatalf("base = %q, want %q", a.apiBase, tc.wantURL)
			}
			if tc.name == "default" && a.apiBase != defaultAPIBase {
				t.Fatalf("default base = %q", a.apiBase)
			}
		})
	}
}

func TestConfigFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "gpunz", "config.json")
	if err := writeConfig(path, fileConfig{APIKey: "gpz_x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestGlobalFlagsAnywhere(t *testing.T) {
	g, rest, err := parseGlobals([]string{"search", "--json", "--api-url=http://x", "--gpu", "a100", "--api-key", "gpz_k", "--", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !g.json || g.apiURL != "http://x" || g.apiKey != "gpz_k" {
		t.Fatalf("globals = %+v", g)
	}
	want := []string{"search", "--gpu", "a100", "--", "--json"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Fatalf("rest = %q, want %q", rest, want)
	}
}

func TestParseArgsInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	tmpl := fs.String("template", "", "")
	disk := fs.Int("disk", 0, "")
	pos, err := parseArgs(fs, []string{"123", "--template", "ubuntu", "--disk", "50", "extra"})
	if err != nil {
		t.Fatal(err)
	}
	if *tmpl != "ubuntu" || *disk != 50 {
		t.Fatalf("flags not parsed: %q %d", *tmpl, *disk)
	}
	if strings.Join(pos, ",") != "123,extra" {
		t.Fatalf("positionals = %v", pos)
	}
}

func TestMoneyFormatting(t *testing.T) {
	// rubles keep the CLI look exactly
	if got := formatMoney(2540, curRUB, false, "/h"); got != "₽25.40/h" {
		t.Fatalf("en rate = %q", got)
	}
	if got := formatMoney(2540, curRUB, true, "/ч"); got != "25,40 ₽/ч" {
		t.Fatalf("ru rate = %q", got)
	}
	if got := formatMoney(123456789, curRUB, true, ""); got != "1"+thinSpace+"234"+thinSpace+"567,89 ₽" {
		t.Fatalf("ru grouped = %q", got)
	}
	if got := formatMoney(-150, curRUB, false, ""); got != "-₽1.50" {
		t.Fatalf("negative = %q", got)
	}
	// a missing or unknown currency (an older server) reads as rubles
	if got := formatMoney(2540, "", false, "/h"); got != "₽25.40/h" {
		t.Fatalf("absent currency = %q", got)
	}
}

func TestDollarFormatting(t *testing.T) {
	cases := []struct {
		name  string
		units int64
		ru    bool
		unit  string
		want  string
	}{
		{"amount en", 125000, false, "", "$12.50"},
		{"amount ru", 125000, true, "", "12,50 $"},
		{"negative en", -125000, false, "", "-$12.50"},
		{"negative ru", -125000, true, "", "-12,50 $"},
		{"zero", 0, false, "", "$0.00"},
		{"cent rounding", 99, false, "", "$0.01"},
		{"grouped en", 123456789012, false, "", "$12" + thinSpace + "345" + thinSpace + "678.90"},
		{"grouped ru", 123456789012, true, "", "12" + thinSpace + "345" + thinSpace + "678,90 $"},
		{"rate under $1 keeps 3 decimals en", 840, false, "/h", "$0.084/h"},
		{"rate under $1 ru", 840, true, "/ч", "0,084 $/ч"},
		{"rate drops a trailing zero to 2 decimals", 800, false, "/h", "$0.08/h"},
		{"rate 0.90 keeps 2 decimals", 9000, false, "/h", "$0.90/h"},
		{"rate from $1 keeps cents", 10900, false, "/h", "$1.09/h"},
		{"zero rate", 0, false, "/h", "$0.00/h"},
	}
	for _, tc := range cases {
		if got := formatMoney(tc.units, curUSD, tc.ru, tc.unit); got != tc.want {
			t.Errorf("%s: formatMoney(%d) = %q, want %q", tc.name, tc.units, got, tc.want)
		}
	}
}

func TestHourlyDollarRounding(t *testing.T) {
	// the site's rule: up to $0.001 below $1, up to $0.01 from $1, always upward
	cases := []struct {
		kopH   int64
		usdRub float64
		want   int64
	}{
		{0, 83.8675, 0},
		{840, 100, 840},       // $0.084 exactly
		{8401, 100, 8410},     // $0.8401 rounds up to $0.841
		{9999, 100, 10000},    // $0.9999 rounds up to $1.00
		{10000, 100, 10000},   // $1.00 exactly
		{99999, 100, 100000},  // from $1 the step is $0.01: $9.9999 rounds up to $10.00
		{2540, 83.8675, 3030}, // ₽25.40 at 83.8675 is $0.303/h
		{1200, 83.8675, 1440}, // ₽12.00 is $0.144/h
	}
	for _, tc := range cases {
		if got := hourlyUSD(tc.kopH, tc.usdRub); got != tc.want {
			t.Errorf("hourlyUSD(%d, %v) = %d, want %d", tc.kopH, tc.usdRub, got, tc.want)
		}
	}
}

func TestHourlyDollarRoundingBounds(t *testing.T) {
	// the converted price is never below the exact one and never more than one step above it, and it never falls as the
	// ruble price rises
	for _, usdRub := range []float64{10, 83.8675, 100, 999.5} {
		var prev int64
		for kopH := int64(1); kopH <= 30000; kopH++ {
			got := hourlyUSD(kopH, usdRub)
			exact := float64(kopH) * 100 / usdRub
			step := 100.0
			if exact < 10000 {
				step = 10
			}
			if float64(got) < exact-1e-6 || float64(got) >= exact+step {
				t.Fatalf("usdRub %v, kop %d: got %d for exact %.4f (step %v)", usdRub, kopH, got, exact, step)
			}
			if got < prev {
				t.Fatalf("usdRub %v: price fell at kop %d", usdRub, kopH)
			}
			prev = got
		}
	}
}

func TestUsableRateAndCatalogFX(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		fx   *fxInfo
		want bool
	}{
		{"no fx", nil, false},
		{"null rate", &fxInfo{Currency: "RUB"}, false},
		{"zero", &fxInfo{Currency: "RUB", USDRub: ptr(0)}, false},
		{"implausibly low", &fxInfo{Currency: "RUB", USDRub: ptr(9)}, false},
		{"implausibly high", &fxInfo{Currency: "RUB", USDRub: ptr(1001)}, false},
		{"not rubles", &fxInfo{Currency: "USD", USDRub: ptr(83)}, false},
		{"published", &fxInfo{Currency: "RUB", USDRub: ptr(83.8675)}, true},
		{"published without a base", &fxInfo{USDRub: ptr(83.8675)}, true},
	}
	for _, tc := range cases {
		if _, ok := usableRate(tc.fx); ok != tc.want {
			t.Errorf("%s: usable = %v, want %v", tc.name, ok, tc.want)
		}
	}
	if fx := catalogFX([]byte(`{"data":[],"next_cursor":null,"fx":{"currency":"RUB","usd_rub":83.8675}}`)); fx == nil || fx.USDRub == nil || *fx.USDRub != 83.8675 {
		t.Fatalf("fx not parsed: %+v", fx)
	}
	if fx := catalogFX([]byte(`{"data":[],"fx":{"currency":"RUB","usd_rub":null}}`)); fx == nil || fx.USDRub != nil {
		t.Fatalf("null rate parsed wrong: %+v", fx)
	}
	if catalogFX([]byte(`[{"slug":"x"}]`)) != nil || catalogFX([]byte(`{"data":[]}`)) != nil {
		t.Fatal("responses without fx must yield nil")
	}
}

func TestLocaleDetection(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if !detectRu(env(map[string]string{"LANG": "ru_RU.UTF-8"}), "") {
		t.Fatal("LANG=ru_RU should be Russian")
	}
	if detectRu(env(map[string]string{"LC_ALL": "C", "LANG": "ru_RU.UTF-8"}), "") {
		t.Fatal("LC_ALL=C should win over LANG")
	}
	if !detectRu(env(map[string]string{}), "ru-RU") {
		t.Fatal("system locale ru-RU should be Russian when env is unset")
	}
}

func TestVisibleWidthIgnoresColour(t *testing.T) {
	if w := visWidth("\x1b[31mabc\x1b[0m"); w != 3 {
		t.Fatalf("visWidth = %d", w)
	}
}

// --- login ----------------------------------------------------------------

func TestLoginDeviceFlow(t *testing.T) {
	polls := 0
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/device": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_name"] == "" {
				t.Errorf("client_name missing")
			}
			writeJSON(w, 200, map[string]any{
				"device_code": "dev-123", "user_code": "ABCD-EFGH",
				"verification_uri":          "https://gpu.nz/console/cli/auth",
				"verification_uri_complete": "https://gpu.nz/console/cli/auth?code=ABCD-EFGH",
				"expires_in":                600, "interval": 1,
			})
		},
		"POST /api/v1/auth/device/token": func(w http.ResponseWriter, r *http.Request) {
			polls++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["device_code"] != "dev-123" {
				t.Errorf("device_code = %q", body["device_code"])
			}
			switch polls {
			case 1:
				apiError(w, 400, "authorization_pending", "pending")
			case 2:
				apiError(w, 400, "slow_down", "slow down")
			default:
				writeJSON(w, 200, map[string]any{"api_key": "gpz_" + strings.Repeat("a", 40), "key_id": "k1", "user": map[string]any{"email": "dev@example.com"}})
			}
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL}, "")
	if code := h.run("login"); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, h.errb)
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
	out := h.out.String()
	if !strings.Contains(out, "ABCD-EFGH") || !strings.Contains(out, "dev@example.com") {
		t.Fatalf("output missing code or email:\n%s", out)
	}
	if len(h.opened) != 1 || h.opened[0] != "https://gpu.nz/console/cli/auth?code=ABCD-EFGH" {
		t.Fatalf("browser opened = %v", h.opened)
	}
	fc, err := readConfig(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fc.APIKey, "gpz_") {
		t.Fatalf("saved key = %q", fc.APIKey)
	}
}

func TestLoginDeniedAndNoBrowser(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/auth/device": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"device_code": "d", "user_code": "AAAA-BBBB", "verification_uri": "https://gpu.nz/console/cli/auth", "expires_in": 600, "interval": 1})
		},
		"POST /api/v1/auth/device/token": func(w http.ResponseWriter, r *http.Request) {
			apiError(w, 400, "access_denied", "denied")
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL}, "")
	if code := h.run("login", "--no-browser"); code != exitAuth {
		t.Fatalf("exit %d, want %d", code, exitAuth)
	}
	if len(h.opened) != 0 {
		t.Fatal("browser should not open with --no-browser")
	}
	if _, err := os.Stat(h.cfg); !os.IsNotExist(err) {
		t.Fatal("config must not be written when login is denied")
	}
}

func TestLoginWithKey(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/me": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer gpz_good" {
				writeJSON(w, 200, map[string]any{"id": "u1", "email": "k@example.com", "two_factor": false})
				return
			}
			apiError(w, 401, "unauthorized", "bad key")
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL}, "")
	if code := h.run("login", "--key", "gpz_bad"); code != exitAuth {
		t.Fatalf("bad key exit %d, want %d", code, exitAuth)
	}
	if _, err := os.Stat(h.cfg); !os.IsNotExist(err) {
		t.Fatal("bad key must not be saved")
	}
	if code := h.run("login", "--key", "gpz_good"); code != 0 {
		t.Fatalf("good key exit %d: %s", code, h.errb)
	}
	if !strings.Contains(h.out.String(), "k@example.com") {
		t.Fatalf("output: %s", h.out)
	}
	fc, _ := readConfig(h.cfg)
	if fc.APIKey != "gpz_good" {
		t.Fatalf("saved %q", fc.APIKey)
	}
}

// --- search and rendering -------------------------------------------------

func TestSearchRenderingAndQuery(t *testing.T) {
	var gotQuery url.Values
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/offers": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			writeJSON(w, 200, map[string]any{"data": []any{sampleOffer("off-1", 2540), sampleOffer("off-2", 61000)}, "next_cursor": nil})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL}, "")
	if code := h.run("search", "--gpu", "rtx-4090", "--max-price", "30", "--min-reliability", "99", "--order", "-price", "--verified"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	if gotQuery.Get("gpu") != "rtx-4090" || gotQuery.Get("max_price_kop_h") != "3000" ||
		gotQuery.Get("min_reliability") != "0.99" || gotQuery.Get("order") != "-price" ||
		gotQuery.Get("verified") != "true" || gotQuery.Get("type") != "on_demand" {
		t.Fatalf("query = %v", gotQuery)
	}
	out := h.out.String()
	for _, want := range []string{"RTX 4090 ×2", "24 GB", "99.2%", "₽25.40/h", "₽610.00/h", "off-1", "ON-DEMAND"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// Columns must line up: header and rows start their second column at the same offset.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("too few lines:\n%s", out)
	}
	if strings.Index(lines[0], "GPU") != strings.Index(lines[1], "RTX") {
		t.Fatalf("misaligned table:\n%s", out)
	}
}

func TestSearchRussianOutput(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/offers": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"data": []any{sampleOffer("off-1", 2540)}, "next_cursor": "c2"})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "LANG": "ru_RU.UTF-8"}, "")
	if code := h.run("search"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "25,40 ₽/ч") || !strings.Contains(h.out.String(), "ПО ТРЕБОВАНИЮ") {
		t.Fatalf("russian output wrong:\n%s", h.out)
	}
}

func TestSearchRejectsBadNumGPUs(t *testing.T) {
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": "http://127.0.0.1:1"}, "")
	if code := h.run("search", "--num-gpus", "3"); code != exitUsage {
		t.Fatalf("exit %d, want usage", code)
	}
}

// --- create and idempotency ----------------------------------------------

func TestCreateSendsIdempotencyKeyAndBody(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var bodies []map[string]any
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/templates": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"slug": "pytorch", "name": "PyTorch", "recommended_disk_gb": 80}}, "next_cursor": nil})
		},
		"POST /api/v1/instances": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			writeJSON(w, 201, map[string]any{"id": "inst-9", "status": "creating", "name": nil})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("create", "off-1", "--template", "pytorch", "--ssh-key", "sk-1", "--ssh-key", "sk-2", "--name", "train", "--type", "interruptible"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	if code := h.run("create", "off-1", "--template", "pytorch", "--disk", "100"); code != 0 {
		t.Fatalf("second exit %d: %s", code, h.errb)
	}
	if len(keys) != 2 {
		t.Fatalf("requests = %d", len(keys))
	}
	for _, k := range keys {
		if k == "" || len(k) > 64 {
			t.Fatalf("bad Idempotency-Key %q", k)
		}
	}
	if keys[0] == keys[1] {
		t.Fatal("separate invocations should use distinct keys")
	}
	if bodies[0]["disk_gb"] != float64(80) {
		t.Fatalf("default disk not taken from template: %v", bodies[0]["disk_gb"])
	}
	ids, _ := bodies[0]["ssh_key_ids"].([]any)
	if len(ids) != 2 || bodies[0]["type"] != "interruptible" || bodies[0]["name"] != "train" {
		t.Fatalf("body = %v", bodies[0])
	}
	if bodies[1]["disk_gb"] != float64(100) {
		t.Fatalf("explicit disk = %v", bodies[1]["disk_gb"])
	}
	if !strings.Contains(h.out.String(), "gpunz show inst-9") {
		t.Fatalf("output: %s", h.out)
	}
}

// --- errors ---------------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   int
	}{
		{402, "insufficient_funds", exitFunds},
		{404, "not_found", exitNotFound},
		{401, "unauthorized", exitAuth},
		{403, "forbidden", exitAuth},
		{422, "validation_error", exitGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv := fakeAPI(t, map[string]http.HandlerFunc{
				"GET /api/v1/instances/{id}": func(w http.ResponseWriter, r *http.Request) {
					apiError(w, tc.status, tc.code, "нет денег / message here")
				},
			})
			h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k", "LANG": "en_US.UTF-8"}, "")
			if code := h.run("show", "inst-1"); code != tc.want {
				t.Fatalf("exit %d, want %d", code, tc.want)
			}
			if !strings.Contains(h.errb.String(), "message here") || !strings.Contains(h.errb.String(), tc.code) {
				t.Fatalf("stderr = %q", h.errb)
			}
		})
	}
}

func TestErrorJSONMode(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/balance": func(w http.ResponseWriter, r *http.Request) {
			apiError(w, 401, "unauthorized", "bad key")
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("--json", "balance"); code != exitAuth {
		t.Fatalf("exit %d", code)
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(h.errb.Bytes(), &env); err != nil || env.Error.Code != "unauthorized" {
		t.Fatalf("stderr JSON = %q (%v)", h.errb, err)
	}
}

func TestNotSignedInDoesNoRequest(t *testing.T) {
	hits := 0
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) { hits++ },
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL}, "")
	if code := h.run("instances"); code != exitAuth {
		t.Fatalf("exit %d, want %d", code, exitAuth)
	}
	if hits != 0 {
		t.Fatal("no request should be sent without a key")
	}
}

func TestNetworkErrorExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": base}, "")
	if code := h.run("gpus"); code != exitGeneric {
		t.Fatalf("exit %d, want %d", code, exitGeneric)
	}
	if !strings.Contains(h.errb.String(), "cannot reach") {
		t.Fatalf("stderr = %q", h.errb)
	}
}

// --- destroy confirmation -------------------------------------------------

func TestDestroyConfirmation(t *testing.T) {
	deletes := 0
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /api/v1/instances/{id}": func(w http.ResponseWriter, r *http.Request) {
			deletes++
			writeJSON(w, 200, map[string]any{"id": r.PathValue("id"), "status": "destroying"})
		},
	})
	env := map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}

	h := newHarness(t, env, "n\n")
	if code := h.run("destroy", "inst-1"); code != 0 || deletes != 0 {
		t.Fatalf("declined: exit %d, deletes %d", code, deletes)
	}
	h = newHarness(t, env, "")
	if code := h.run("destroy", "inst-1"); code != 0 || deletes != 0 {
		t.Fatalf("empty input must cancel: exit %d, deletes %d", code, deletes)
	}
	h = newHarness(t, env, "y\n")
	if code := h.run("destroy", "inst-1"); code != 0 || deletes != 1 {
		t.Fatalf("confirmed: exit %d, deletes %d", code, deletes)
	}
	h = newHarness(t, env, "")
	if code := h.run("destroy", "-y", "inst-1"); code != 0 || deletes != 2 {
		t.Fatalf("-y: exit %d, deletes %d", code, deletes)
	}
}

// --- logs and instances -----------------------------------------------------

func TestLogsTailParameter(t *testing.T) {
	var tail string
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/instances/{id}/logs": func(w http.ResponseWriter, r *http.Request) {
			tail = r.URL.Query().Get("tail")
			writeJSON(w, 200, map[string]any{"lines": []string{"epoch 1", "epoch 2"}})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("logs", "inst-1", "--tail", "25"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if tail != "25" || h.out.String() != "epoch 1\nepoch 2\n" {
		t.Fatalf("tail=%q out=%q", tail, h.out)
	}
}

func TestInstancesTable(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/instances": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("status") != "all" {
				t.Errorf("--all should send status=all")
			}
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{
				"id": "inst-1", "name": "train", "status": "running", "type": "on_demand",
				"gpu":             map[string]any{"name": "A100", "count": 1},
				"template":        map[string]any{"slug": "pytorch"},
				"price":           map[string]any{"running_kop_h": 9900},
				"cost_so_far_kop": 12345, "created_at": "2026-10-10T10:00:00Z",
			}}, "next_cursor": nil})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("ls", "--all"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"inst-1", "A100", "₽99.00/h", "₽123.45"} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, h.out)
		}
	}
}

// --- ssh keys -------------------------------------------------------------

func TestSSHKeyAddReadsFile(t *testing.T) {
	var got map[string]string
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/ssh-keys": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			writeJSON(w, 201, map[string]any{"id": "sk-1", "name": got["name"], "type": "ssh-ed25519", "fingerprint": "SHA256:abc"})
		},
	})
	pub := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAC3 laptop@host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("ssh-keys", "add", pub); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	if got["name"] != "laptop@host" || got["public_key"] != "ssh-ed25519 AAAAC3 laptop@host" {
		t.Fatalf("posted %v", got)
	}
}

// --- update checksum --------------------------------------------------------

func serveRelease(t *testing.T, binary []byte, sumOverride string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	hexSum := hex.EncodeToString(sum[:])
	if sumOverride != "" {
		hexSum = sumOverride
	}
	name := binaryName(runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/VERSION", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "9.9.9\n") })
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  gpunz-other\n%s  %s\n", strings.Repeat("0", 64), hexSum, name)
	})
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateVerifiesChecksumAndReplaces(t *testing.T) {
	binary := []byte("new-binary-contents")
	srv := serveRelease(t, binary, "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "gpunz")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil, "")
	h.a.updateBase = srv.URL + "/"
	h.a.exePath = func() (string, error) { return exe, nil }
	if code := h.run("update"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(binary) {
		t.Fatalf("binary not replaced: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
}

func TestUpdateRejectsBadChecksum(t *testing.T) {
	srv := serveRelease(t, []byte("payload"), strings.Repeat("a", 64))
	dir := t.TempDir()
	exe := filepath.Join(dir, "gpunz")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil, "")
	h.a.updateBase = srv.URL + "/"
	h.a.exePath = func() (string, error) { return exe, nil }
	if code := h.run("update"); code == 0 {
		t.Fatal("update must fail on checksum mismatch")
	}
	if !strings.Contains(h.errb.String(), "checksum mismatch") {
		t.Fatalf("stderr = %q", h.errb)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old" {
		t.Fatal("binary must not change when verification fails")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind after failed verification: %d entries", len(entries))
	}
}

// --- misc -------------------------------------------------------------------

func TestCompletionScripts(t *testing.T) {
	h := newHarness(t, nil, "")
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		if code := h.run("completion", sh); code != 0 {
			t.Fatalf("%s exit %d", sh, code)
		}
		if h.out.Len() == 0 || !strings.Contains(h.out.String(), "gpunz") {
			t.Fatalf("%s script empty", sh)
		}
	}
	if code := h.run("completion", "tcsh"); code != exitUsage {
		t.Fatalf("unknown shell exit %d", code)
	}
}

func TestVersionAndUnknownCommand(t *testing.T) {
	h := newHarness(t, nil, "")
	if code := h.run("version"); code != 0 || !strings.Contains(h.out.String(), "gpunz ") {
		t.Fatalf("version exit %d out %q", code, h.out)
	}
	if code := h.run("bogus"); code != exitUsage {
		t.Fatalf("unknown command exit %d", code)
	}
}

func TestLogoutClearsKey(t *testing.T) {
	h := newHarness(t, nil, "")
	if err := writeConfig(h.cfg, fileConfig{APIKey: "gpz_x", APIURL: "http://x/api/v1"}); err != nil {
		t.Fatal(err)
	}
	if code := h.run("logout"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fc, _ := readConfig(h.cfg)
	if fc.APIKey != "" || fc.APIURL != "http://x/api/v1" {
		t.Fatalf("after logout: %+v", fc)
	}
}

// --- account currency -------------------------------------------------------

// balanceBody is a GET /balance answer for an account in currency; "" leaves the field out, as older servers did.
func balanceBody(currency string) map[string]any {
	body := map[string]any{"balance_kop": 1234500, "available_kop": 1234500, "credit_limit_kop": 0, "burn_kop_h": 100, "runway_hours": nil}
	if currency != "" {
		body["currency"] = currency
	}
	return body
}

// balanceReply serves GET /balance and counts the reads.
func balanceReply(hits *int, currency string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*hits++
		writeJSON(w, 200, balanceBody(currency))
	}
}

// offersReply serves one offer (₽25.40/h on demand, ₽12.00/h interruptible) with the catalog rate usdRub.
func offersReply(usdRub any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"data":        []any{sampleOffer("off-1", 2540)},
			"next_cursor": nil,
			"fx":          map[string]any{"currency": "RUB", "usd_rub": usdRub},
		})
	}
}

func TestBalanceShowsAccountCurrency(t *testing.T) {
	cases := []struct {
		name     string
		currency string
		want     []string
	}{
		{"dollars", "USD", []string{"$123.45", "$0.01/h"}},
		{"rubles", "RUB", []string{"₽12" + thinSpace + "345.00", "₽1.00/h"}},
		{"older server, no currency", "", []string{"₽12" + thinSpace + "345.00"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := fakeAPI(t, map[string]http.HandlerFunc{"GET /api/v1/balance": balanceReply(&hits, tc.currency)})
			h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
			if code := h.run("balance"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb)
			}
			for _, want := range tc.want {
				if !strings.Contains(h.out.String(), want) {
					t.Fatalf("missing %q:\n%s", want, h.out)
				}
			}
		})
	}
}

func TestInstancesShowAccountCurrency(t *testing.T) {
	inst := map[string]any{
		"id": "inst-1", "name": "train", "status": "running", "type": "on_demand",
		"gpu":             map[string]any{"name": "A100", "count": 1, "vram_gb": 80},
		"template":        map[string]any{"slug": "pytorch", "name": "PyTorch"},
		"price":           map[string]any{"running_kop_h": 9900, "stopped_kop_h": 0, "traffic_kop_gb": 0},
		"cost_so_far_kop": 123450, "currency": "USD", "created_at": "2026-10-10T10:00:00Z",
	}
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/instances": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"data": []any{inst}, "next_cursor": nil})
		},
		"GET /api/v1/instances/{id}": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, inst)
		},
	})
	env := map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}
	h := newHarness(t, env, "")
	if code := h.run("ls"); code != 0 {
		t.Fatalf("ls exit %d: %s", code, h.errb)
	}
	for _, want := range []string{"$0.99/h", "$12.35"} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("ls missing %q:\n%s", want, h.out)
		}
	}
	h = newHarness(t, env, "")
	if code := h.run("show", "inst-1"); code != 0 {
		t.Fatalf("show exit %d: %s", code, h.errb)
	}
	for _, want := range []string{"$0.99/h running, $0.00/h stopped", "$12.35"} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("show missing %q:\n%s", want, h.out)
		}
	}
}

func TestPriceChangedShowsAmountsInAccountCurrency(t *testing.T) {
	cases := []struct {
		name    string
		lang    string
		details map[string]any
		want    string
	}{
		{"rubles by default", "", map[string]any{"current_kop_h": 2540, "new_kop_h": 2600}, "₽25.40/h → ₽26.00/h"},
		{"rubles in Russian", "ru_RU.UTF-8", map[string]any{"current_kop_h": 2540, "new_kop_h": 2600, "currency": "RUB"}, "25,40 ₽/ч → 26,00 ₽/ч"},
		{"dollars", "", map[string]any{"current_kop_h": 9900, "new_kop_h": 10900, "currency": "USD"}, "$0.99/h → $1.09/h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeAPI(t, map[string]http.HandlerFunc{
				"POST /api/v1/instances/{id}/start": func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, 409, map[string]any{"error": map[string]any{
						"code": "price_changed", "message": "The price changed", "details": tc.details,
					}})
				},
			})
			env := map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}
			if tc.lang != "" {
				env["LANG"] = tc.lang
			}
			h := newHarness(t, env, "")
			if code := h.run("start", "inst-1"); code == 0 {
				t.Fatal("price_changed must exit non-zero")
			}
			if !strings.Contains(h.out.String(), tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, h.out)
			}
		})
	}
}

func TestSearchShowsDollarsForUSDAccount(t *testing.T) {
	balance := 0
	maxKop := ""
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/balance": balanceReply(&balance, "USD"),
		"GET /api/v1/offers": func(w http.ResponseWriter, r *http.Request) {
			maxKop = r.URL.Query().Get("max_price_kop_h")
			offersReply(83.8675)(w, r)
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("search", "--max-price", "30"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	out := h.out.String()
	for _, want := range []string{"$0.303/h", "$0.144/h"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "₽") {
		t.Fatalf("dollar account shows rubles:\n%s", out)
	}
	// --max-price stays in rubles on every account: 30 ₽ is the market filter 3000
	if maxKop != "3000" || balance != 1 {
		t.Fatalf("max_price_kop_h = %q, balance reads = %d", maxKop, balance)
	}
}

func TestGpusShowsDollarsInRussian(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/balance": balanceReply(new(int), "USD"),
		"GET /api/v1/gpus": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{
				"data": []any{map[string]any{
					"slug": "rtx-4090", "name": "RTX 4090", "vram_gb": 24, "tier": "consumer", "available_gpus": 3,
					"from_kop_h": 2540, "from_interruptible_kop_h": nil, "currency": "RUB",
				}},
				"next_cursor": nil,
				"fx":          map[string]any{"currency": "RUB", "usd_rub": 83.8675},
			})
		},
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k", "LANG": "ru_RU.UTF-8"}, "")
	if code := h.run("gpus"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	out := h.out.String()
	if !strings.Contains(out, "0,303 $/ч") || strings.Contains(out, "₽") {
		t.Fatalf("russian dollar catalog wrong:\n%s", out)
	}
}

func TestCatalogStaysInRublesWhenDollarsCannotBeBacked(t *testing.T) {
	cases := []struct {
		name         string
		key          bool
		usdRub       any
		currency     string
		balanceCode  int
		wantBalances int
	}{
		{"anonymous reads no balance", false, 83.8675, "USD", 0, 0},
		{"no published rate reads no balance", true, nil, "USD", 0, 0},
		{"implausible rate", true, 5.0, "USD", 0, 0},
		{"rubles account", true, 83.8675, "RUB", 0, 1},
		{"older server without currency", true, 83.8675, "", 0, 1},
		{"balance unreadable", true, 83.8675, "", http.StatusForbidden, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			balances := 0
			srv := fakeAPI(t, map[string]http.HandlerFunc{
				"GET /api/v1/balance": func(w http.ResponseWriter, r *http.Request) {
					balances++
					if tc.balanceCode != 0 {
						apiError(w, tc.balanceCode, "forbidden", "key lacks billing:read")
						return
					}
					writeJSON(w, 200, balanceBody(tc.currency))
				},
				"GET /api/v1/offers": offersReply(tc.usdRub),
			})
			env := map[string]string{"GPUNZ_API_URL": srv.URL}
			if tc.key {
				env["GPUNZ_API_KEY"] = "gpz_k"
			}
			h := newHarness(t, env, "")
			if code := h.run("search"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb)
			}
			out := h.out.String()
			if !strings.Contains(out, "₽25.40/h") || strings.Contains(out, "$") {
				t.Fatalf("expected rubles:\n%s", out)
			}
			if balances != tc.wantBalances {
				t.Fatalf("balance reads = %d, want %d", balances, tc.wantBalances)
			}
		})
	}
}

func TestJSONOutputReadsNoBalance(t *testing.T) {
	balances := 0
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/balance": balanceReply(&balances, "USD"),
		"GET /api/v1/offers":  offersReply(83.8675),
	})
	h := newHarness(t, map[string]string{"GPUNZ_API_URL": srv.URL, "GPUNZ_API_KEY": "gpz_k"}, "")
	if code := h.run("--json", "search"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	if balances != 0 {
		t.Fatalf("JSON output read the balance %d times", balances)
	}
	if !strings.Contains(h.out.String(), `"usd_rub"`) {
		t.Fatalf("JSON should pass the response through:\n%s", h.out)
	}
}

func TestTopupURL(t *testing.T) {
	cases := []struct {
		base   string
		ru     bool
		amount string
		want   string
	}{
		{"https://gpu.nz/api/v1", true, "500", "https://gpu.nz/console/billing?topup=500"},
		{"https://gpu.nz/api/v1", false, "", "https://gpu.nz/en/console/billing"},
		{"http://localhost:3000/api/v1", true, "", "http://localhost:3000/console/billing"},
		{"::bad", true, "100", "https://gpu.nz/console/billing?topup=100"},
	}
	for _, c := range cases {
		if got := topupURL(c.base, c.ru, c.amount); got != c.want {
			t.Fatalf("topupURL(%q, %v, %q) = %q, want %q", c.base, c.ru, c.amount, got, c.want)
		}
	}
}

func TestTopupPrintsLinkAndQR(t *testing.T) {
	h := newHarness(t, nil, "")
	opened := ""
	h.a.openURL = func(u string) error { opened = u; return nil }
	if code := h.run("topup", "750"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb)
	}
	out := h.out.String()
	if !strings.Contains(out, "/console/billing?topup=750") || !strings.Contains(out, "█") {
		t.Fatalf("output has no link or QR: %q", out)
	}
	if !strings.HasSuffix(opened, "/console/billing?topup=750") {
		t.Fatalf("opened %q", opened)
	}
	if code := h.run("topup", "12.5"); code == 0 {
		t.Fatal("a fractional amount was accepted")
	}
}
