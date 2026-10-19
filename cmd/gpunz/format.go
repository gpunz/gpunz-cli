package main

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const thinSpace = " "

// paint wraps text in ANSI colours when colour is enabled.
type paint struct{ on bool }

func (p paint) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p paint) bold(s string) string   { return p.wrap("1", s) }
func (p paint) dim(s string) string    { return p.wrap("2", s) }
func (p paint) red(s string) string    { return p.wrap("31", s) }
func (p paint) green(s string) string  { return p.wrap("32", s) }
func (p paint) yellow(s string) string { return p.wrap("33", s) }
func (p paint) cyan(s string) string   { return p.wrap("36", s) }

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visWidth counts printed columns, ignoring ANSI colour codes.
func visWidth(s string) int {
	return utf8.RuneCountInString(ansiRe.ReplaceAllString(s, ""))
}

// table renders left-aligned, space-separated columns.
type table struct {
	head []string
	rows [][]string
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(w io.Writer, p paint) {
	widths := make([]int, len(t.head))
	for i, h := range t.head {
		widths[i] = visWidth(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if i < len(widths) && visWidth(c) > widths[i] {
				widths[i] = visWidth(c)
			}
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i, c := range cells {
			b.WriteString(style(c))
			if i < len(cells)-1 && i < len(widths) {
				b.WriteString(strings.Repeat(" ", widths[i]-visWidth(c)+2))
			}
		}
		fmt.Fprintln(w, b.String())
	}
	line(t.head, p.bold)
	for _, r := range t.rows {
		line(r, func(s string) string { return s })
	}
}

func groupThousands(n int64, sep string) string {
	s := strconv.FormatInt(n, 10)
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	var b strings.Builder
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteString(sep)
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// Currency codes of account and catalog money. The API sends "RUB" or "USD"; an older server may send none.
const (
	curRUB = "RUB"
	curUSD = "USD"
)

// formatMoney renders ledger units of a currency: kopecks for rubles, 1/10 000 of a dollar for dollars. Any currency
// other than USD, including the empty one of an older server, is rubles. A unit suffix such as "/h" marks an hourly
// rate.
//
// Rubles: "1 234,50 ₽" in Russian (+unit, e.g. "/ч") and "₽1 234.50" in English (+unit, e.g. "/h"); digits group with
// a thin space in both languages. Dollars: "12,50 $" in Russian and "$12.50" in English. A dollar rate under $1 keeps
// a third decimal: "$0.084/h", never "$0.08/h".
func formatMoney(units int64, currency string, ru bool, unit string) string {
	sign := ""
	if units < 0 {
		sign = "-"
		units = -units
	}
	dec := "."
	if ru {
		dec = ","
	}
	if currency == curUSD {
		num := usdDigits(units, dec, unit != "")
		if ru {
			return sign + num + " $" + unit
		}
		return sign + "$" + num + unit
	}
	num := groupThousands(units/100, thinSpace) + dec + fmt.Sprintf("%02d", units%100)
	if ru {
		return sign + num + " ₽" + unit
	}
	return sign + "₽" + num + unit
}

// usdDigits renders dollar ledger units (1/10 000 $) without the sign. An amount keeps cents, rounded half up. A rate
// (rate true) under $1 keeps a third decimal, rounded half up to a tenth of a cent, and drops trailing zeros down to
// two digits.
func usdDigits(units int64, dec string, rate bool) string {
	if rate && units < 10000 {
		milli := (units + 5) / 10
		frac := fmt.Sprintf("%03d", milli%1000)
		for len(frac) > 2 && frac[len(frac)-1] == '0' {
			frac = frac[:len(frac)-1]
		}
		return strconv.FormatInt(milli/1000, 10) + dec + frac
	}
	cents := (units + 50) / 100
	return groupThousands(cents/100, thinSpace) + dec + fmt.Sprintf("%02d", cents%100)
}

// hourlyUSD converts a ruble hourly price (kopecks per hour) to dollar ledger units at usdRub rubles per dollar. It is
// the site's rule: the price rounds UP to $0.001 below $1 and to $0.01 from $1, so a
// converted price never falls under the ruble one. The caller passes a plausible rate.
func hourlyUSD(kopH int64, usdRub float64) int64 {
	if kopH <= 0 {
		return 0
	}
	units := float64(kopH) * 100 / usdRub
	step := 100.0
	if units < 10000 {
		step = 10
	}
	return int64(math.Ceil(units/step-1e-9) * step)
}

// rate formats an hourly price: units of currency per hour.
func (a *app) rate(units int64, currency string) string {
	return formatMoney(units, currency, a.ru, a.tr("/h", "/ч"))
}

// money formats an amount: units of currency.
func (a *app) money(units int64, currency string) string {
	return formatMoney(units, currency, a.ru, "")
}

// humanMinutes renders a span such as "2d 5h", "3h 12min" or "45min".
func (a *app) humanMinutes(mins int64) string {
	if mins < 0 {
		mins = 0
	}
	d, h, m := mins/1440, (mins%1440)/60, mins%60
	dU, hU, mU := a.tr("d", "д"), a.tr("h", "ч"), a.tr("min", "мин")
	switch {
	case d > 0 && h > 0:
		return fmt.Sprintf("%d%s %d%s", d, dU, h, hU)
	case d > 0:
		return fmt.Sprintf("%d%s", d, dU)
	case h > 0:
		return fmt.Sprintf("%d%s %d%s", h, hU, m, mU)
	}
	return fmt.Sprintf("%d%s", m, mU)
}

// runway formats hours (may be nil for "no burn").
func (a *app) runway(hours *float64) string {
	if hours == nil {
		return "—"
	}
	return a.humanMinutes(int64(math.Round(*hours * 60)))
}

// timeLocal formats an RFC 3339 timestamp in local time.
func timeLocal(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ago formats how long before now a timestamp was, e.g. "3h 2min ago".
func (a *app) ago(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	d := int64(a.now().Sub(t).Minutes())
	return a.tr(a.humanMinutes(d)+" ago", a.humanMinutes(d)+" назад")
}

func strOr(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

func intOr(p *int, def string) string {
	if p == nil {
		return def
	}
	return strconv.Itoa(*p)
}

func (a *app) statusStyle(status string) string {
	switch status {
	case "running":
		return a.p.green(status)
	case "failed", "outbid", "destroyed":
		return a.p.red(status)
	case "stopped":
		return a.p.dim(status)
	case "creating", "starting", "stopping", "destroying":
		return a.p.yellow(status)
	}
	return status
}

// kv prints an aligned label/value block.
func kv(w io.Writer, p paint, pairs [][2]string) {
	width := 0
	for _, kvp := range pairs {
		if visWidth(kvp[0]) > width {
			width = visWidth(kvp[0])
		}
	}
	for _, kvp := range pairs {
		fmt.Fprintf(w, "%s%s%s\n", p.dim(kvp[0]), strings.Repeat(" ", width-visWidth(kvp[0])+2), kvp[1])
	}
}
