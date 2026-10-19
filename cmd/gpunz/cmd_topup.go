package main

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"rsc.io/qr"
)

var topupAmountRe = regexp.MustCompile(`^\d{1,7}$`)

// cmdTopup sends the user to the top-up page of the site: the payment itself (SBP, crypto) runs there, where it is
// already checked. The link is printed, drawn as a QR code for a phone camera and opened in the browser.
func cmdTopup(a *app, args []string) error {
	fs := a.flagSet("topup")
	noBrowser := fs.Bool("no-browser", false, "print the link and the QR code without opening a browser")
	noQR := fs.Bool("no-qr", false, "do not draw the QR code")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	amount := ""
	if len(rest) > 1 {
		return usageErr("%s", a.tr("usage: gpunz topup [AMOUNT]", "использование: gpunz topup [СУММА]"))
	}
	if len(rest) == 1 {
		amount = strings.TrimSpace(rest[0])
		if !topupAmountRe.MatchString(amount) || strings.TrimLeft(amount, "0") == "" {
			return usageErr("%s", a.tr("AMOUNT is a whole number, e.g. 500", "СУММА — целое число, например 500"))
		}
	}
	link := topupURL(a.apiBase, a.ru, amount)

	if amount != "" {
		fmt.Fprintf(a.stdout, a.tr("Top-up amount: %s (in your balance currency)\n", "Сумма пополнения: %s (в валюте баланса)\n"), a.p.bold(amount))
	}
	fmt.Fprintln(a.stdout, a.tr("Pay on the site: SBP from any Russian bank or cryptocurrency.", "Оплата на сайте: СБП из любого российского банка или криптовалюта."))
	if !*noQR && !a.g.json {
		fmt.Fprintln(a.stdout, a.tr("Scan with your phone camera:", "Отсканируйте камерой телефона:"))
		if err := writeQR(a.stdout, link, a.p.on); err != nil {
			fmt.Fprintln(a.stderr, a.tr("cannot draw the QR code:", "не удалось нарисовать QR-код:"), err)
		}
	}
	fmt.Fprintln(a.stdout, a.tr("Or open:", "Или откройте:"), link)
	if !*noBrowser {
		if err := a.openURL(link); err == nil {
			fmt.Fprintln(a.stdout, a.tr("Opened the top-up page in your browser.", "Страница пополнения открыта в браузере."))
		}
	}
	fmt.Fprintln(a.stdout, a.tr("Check the balance afterwards: gpunz balance", "Проверить баланс после оплаты: gpunz balance"))
	return nil
}

// topupURL is the console billing page of the site the API lives on, with the amount preselected (?topup=N). English
// users get the /en pages; Russian is served without a prefix.
func topupURL(apiBase string, ru bool, amount string) string {
	origin := "https://gpu.nz"
	if u, err := url.Parse(apiBase); err == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	path := "/console/billing"
	if !ru {
		path = "/en" + path
	}
	if amount != "" {
		return origin + path + "?topup=" + amount
	}
	return origin + path
}

// writeQR draws text as a QR code with half-block characters, two modules per character row. With colour it paints
// dark modules black on white, so it scans on light and dark terminals alike; without colour the light modules are
// full blocks, which suits the usual dark terminal.
func writeQR(w io.Writer, text string, color bool) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	const quiet = 3
	n := code.Size
	dark := func(x, y int) bool {
		if x < 0 || y < 0 || x >= n || y >= n {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < n+quiet; y += 2 {
		b.WriteString("  ")
		for x := -quiet; x < n+quiet; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			if color {
				// upper half in the foreground colour, lower half in the background colour
				fg, bg := 97, 107
				if top {
					fg = 30
				}
				if bottom {
					bg = 40
				}
				fmt.Fprintf(&b, "\x1b[%d;%dm▀", fg, bg)
				continue
			}
			switch {
			case !top && !bottom:
				b.WriteString("█")
			case !top:
				b.WriteString("▀")
			case !bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		if color {
			b.WriteString("\x1b[0m")
		}
		b.WriteString("\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}
