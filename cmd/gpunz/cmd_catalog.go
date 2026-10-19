package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func cmdGpus(a *app, args []string) error {
	fs := a.flagSet("gpus")
	limit := fs.Int("limit", 50, "maximum rows (1-200)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(clampLimit(*limit))}}
	items, _, raw, err := getList[GpuModel](a.client(false), "/gpus", q)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No GPUs available right now.", "Сейчас нет доступных GPU."))
		return nil
	}
	t := table{head: []string{"SLUG", a.tr("GPU", "GPU"), "VRAM", a.tr("TIER", "КЛАСС"), a.tr("FREE", "СВОБОДНО"),
		a.tr("ON-DEMAND FROM", "ОТ (ПО ТРЕБОВАНИЮ)"), a.tr("INTERRUPTIBLE FROM", "ОТ (ПРЕРЫВАЕМЫЕ)")}}
	view := a.catalogView(catalogFX(raw))
	for _, g := range items {
		t.add(g.Slug, g.Name, fmt.Sprintf("%d GB", g.VRAMGB), g.Tier, strconv.Itoa(g.AvailableGpus),
			a.marketRate(view, g.FromKopH), a.marketRate(view, g.FromInterruptibleKopH))
	}
	t.render(a.stdout, a.p)
	return nil
}

func cmdTemplates(a *app, args []string) error {
	if _, err := parseArgs(a.flagSet("templates"), args); err != nil {
		return err
	}
	items, _, raw, err := getList[Template](a.client(true), "/templates", nil)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	t := table{head: []string{"SLUG", a.tr("NAME", "НАЗВАНИЕ"), a.tr("MODE", "РЕЖИМ"), a.tr("PORTS", "ПОРТЫ"),
		a.tr("MIN DISK", "МИН. ДИСК"), a.tr("REC. DISK", "РЕКОМ. ДИСК"), "OWNER"}}
	for _, tp := range items {
		ports := "—"
		if len(tp.Ports) > 0 {
			ps := make([]string, len(tp.Ports))
			for i, p := range tp.Ports {
				ps[i] = strconv.Itoa(p)
			}
			ports = strings.Join(ps, ",")
		}
		t.add(tp.Slug, tp.Name, tp.LaunchMode, ports, fmt.Sprintf("%d GB", tp.MinDiskGB),
			fmt.Sprintf("%d GB", tp.RecommendedDiskGB), tp.Owner)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No templates.", "Шаблонов нет."))
		return nil
	}
	t.render(a.stdout, a.p)
	return nil
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 200 {
		return 200
	}
	return n
}

// searchOpts are the flags of `gpunz search`.
type searchOpts struct {
	gpu, typ, country, order                  string
	numGPUs, minVRAM, minDisk, minDown, limit int
	maxPrice, minReliability                  float64
	minCUDA                                   string
	verified                                  bool
}

func cmdSearch(a *app, args []string) error {
	fs := a.flagSet("search")
	var o searchOpts
	fs.StringVar(&o.gpu, "gpu", "", "GPU model slug, comma-separated (e.g. rtx-4090,a100)")
	fs.IntVar(&o.numGPUs, "num-gpus", 0, "GPUs per machine: 1, 2, 4 or 8")
	fs.StringVar(&o.typ, "type", "on_demand", "on_demand or interruptible")
	fs.Float64Var(&o.maxPrice, "max-price", 0, "maximum on-demand price, rubles per hour (the market unit, also for USD accounts)")
	fs.StringVar(&o.country, "country", "", "ISO-2 country codes, comma-separated")
	fs.Float64Var(&o.minReliability, "min-reliability", 0, "minimum reliability, 0-1 or a percentage (e.g. 0.99 or 99)")
	fs.BoolVar(&o.verified, "verified", false, "only verified machines")
	fs.IntVar(&o.minVRAM, "min-vram", 0, "minimum VRAM per GPU, GB")
	fs.StringVar(&o.minCUDA, "min-cuda", "", "minimum CUDA version, e.g. 12.4")
	fs.IntVar(&o.minDisk, "min-disk", 0, "minimum disk, GB")
	fs.IntVar(&o.minDown, "min-down", 0, "minimum download speed, Mbps")
	fs.StringVar(&o.order, "order", "", "price (default), -price, dlperf, dlperf_per_price, reliability, vram, inet_down")
	fs.IntVar(&o.limit, "limit", 50, "maximum rows (1-200)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	q, err := searchQuery(o)
	if err != nil {
		return err
	}
	items, next, raw, err := getList[Offer](a.client(false), "/offers", q)
	if err != nil {
		return err
	}
	if a.g.json {
		return a.writeRaw(raw)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.stdout, a.tr("No offers match these filters.", "Под эти фильтры предложений нет."))
		return nil
	}
	t := table{head: []string{"ID", "GPU", "VRAM", "CPU", "RAM", a.tr("DISK", "ДИСК"), a.tr("NET ↓/↑", "СЕТЬ ↓/↑"),
		a.tr("RELIAB.", "НАДЁЖН."), a.tr("CTRY", "СТРАНА"), a.tr("ON-DEMAND", "ПО ТРЕБОВАНИЮ"), a.tr("INTERRUPT.", "ПРЕРЫВАЕМЫЙ")}}
	view := a.catalogView(catalogFX(raw))
	for _, off := range items {
		gpu := off.GPU.Name
		if gpu == "" {
			gpu = off.GPU.Slug
		}
		cpu := "—"
		if off.CPU.Cores != nil {
			cpu = fmt.Sprintf("%d vCPU", *off.CPU.Cores)
		}
		ram := "—"
		if off.RAM != nil {
			ram = fmt.Sprintf("%d GB", *off.RAM)
		}
		disk := "—"
		if off.Disk.MaxGB != nil {
			disk = fmt.Sprintf("%d GB", *off.Disk.MaxGB)
		}
		net := "—"
		if off.Network.DownMbps != nil || off.Network.UpMbps != nil {
			net = fmt.Sprintf("%s/%s Mbps", intOr(off.Network.DownMbps, "—"), intOr(off.Network.UpMbps, "—"))
		}
		rel := "—"
		if off.Reliability != nil {
			rel = fmt.Sprintf("%.1f%%", *off.Reliability*100)
		}
		t.add(off.ID, fmt.Sprintf("%s ×%d", gpu, off.GPU.Count), fmt.Sprintf("%d GB", off.GPU.VRAMGB), cpu, ram, disk, net,
			rel, strings.ToUpper(off.Country), a.marketRate(view, &off.Price.OnDemandKopH), a.marketRate(view, off.Price.InterruptibleKopH))
	}
	t.render(a.stdout, a.p)
	fmt.Fprintf(a.stdout, a.tr("\n%d offers shown. Create one with: gpunz create OFFER_ID --template SLUG\n",
		"\nПоказано предложений: %d. Создать: gpunz create OFFER_ID --template SLUG\n"), len(items))
	if next != nil {
		fmt.Fprintln(a.stdout, a.tr("More results exist; raise --limit (max 200) to see them.",
			"Есть ещё результаты; увеличьте --limit (максимум 200)."))
	}
	return nil
}

// searchQuery turns flags into /offers query parameters. Prices are entered in rubles, the market's unit, whatever the
// account's currency: --max-price and the filter stay in rubles.
func searchQuery(o searchOpts) (url.Values, error) {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("gpu", strings.ReplaceAll(o.gpu, " ", ""))
	if o.numGPUs != 0 {
		switch o.numGPUs {
		case 1, 2, 4, 8:
			q.Set("num_gpus", strconv.Itoa(o.numGPUs))
		default:
			return nil, usageErr("--num-gpus must be 1, 2, 4 or 8")
		}
	}
	switch o.typ {
	case "", "on_demand", "interruptible":
		set("type", o.typ)
	default:
		return nil, usageErr("--type must be on_demand or interruptible")
	}
	if o.maxPrice > 0 {
		q.Set("max_price_kop_h", strconv.FormatInt(int64(o.maxPrice*100+0.5), 10))
	}
	set("country", strings.ToUpper(strings.ReplaceAll(o.country, " ", "")))
	if o.minReliability > 0 {
		r := o.minReliability
		if r > 1 {
			r /= 100
		}
		q.Set("min_reliability", strconv.FormatFloat(r, 'f', -1, 64))
	}
	if o.verified {
		q.Set("verified", "true")
	}
	if o.minVRAM > 0 {
		q.Set("min_vram_gb", strconv.Itoa(o.minVRAM))
	}
	set("min_cuda", o.minCUDA)
	if o.minDisk > 0 {
		q.Set("min_disk_gb", strconv.Itoa(o.minDisk))
	}
	if o.minDown > 0 {
		q.Set("min_inet_down_mbps", strconv.Itoa(o.minDown))
	}
	set("order", o.order)
	q.Set("limit", strconv.Itoa(clampLimit(o.limit)))
	return q, nil
}

// priceView is the currency catalog prices show in: rubles, the market's own unit, or dollars converted at usdRub
// rubles per dollar, the rate the site uses.
type priceView struct {
	currency string
	usdRub   float64
}

// units converts a ruble market price (kopecks per hour) into the view's ledger units.
func (v priceView) units(kopH int64) int64 {
	if v.currency == curUSD {
		return hourlyUSD(kopH, v.usdRub)
	}
	return kopH
}

// catalogView decides how a catalog response shows its prices. Dollars need a usable rate in the response and a
// signed-in account whose balance is in dollars; otherwise prices stay in rubles. The balance is read only when a rate
// exists, so an anonymous or rate-less catalog costs no extra request.
func (a *app) catalogView(fx *fxInfo) priceView {
	usdRub, ok := usableRate(fx)
	if !ok || a.accountCurrency() != curUSD {
		return priceView{currency: curRUB}
	}
	return priceView{currency: curUSD, usdRub: usdRub}
}

// usableRate returns the rate a catalog response publishes for dollar prices, when it is plausible. The bounds are the
// site's: the CLI never converts with a made-up rate.
func usableRate(fx *fxInfo) (float64, bool) {
	if fx == nil || fx.USDRub == nil || (fx.Currency != "" && fx.Currency != curRUB) {
		return 0, false
	}
	if r := *fx.USDRub; r >= 10 && r <= 1000 {
		return r, true
	}
	return 0, false
}

// marketRate formats a ruble market price (kopecks per hour) in the view's currency. A nil price shows "—".
func (a *app) marketRate(v priceView, kopH *int64) string {
	if kopH == nil {
		return "—"
	}
	return a.rate(v.units(*kopH), v.currency)
}
