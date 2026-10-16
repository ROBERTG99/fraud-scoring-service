package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

// GenVersion changes whenever the output for a given seed and size changes.
const GenVersion = 1

// Fraud patterns (cmd/gen/PATTERNS.md).
const (
	PatternNone        = "none"
	PatternATO         = "account_takeover"
	PatternCardTesting = "card_testing"
	PatternMule        = "mule_account"
	PatternFriendly    = "friendly_fraud"
)

// FraudRate is the target share of fraudulent transactions.
const FraudRate = 0.008

const (
	minute = int64(60_000)
	hour   = 60 * minute
	day    = 24 * hour
)

// patternShare is the target share of fraudulent transactions per pattern.
var patternShare = []struct {
	name  string
	share float64
}{
	{PatternATO, 0.37}, {PatternCardTesting, 0.27}, {PatternMule, 0.28}, {PatternFriendly, 0.08},
}

// Config controls one dataset.
type Config struct {
	Size        string
	Events      int
	Accounts    int
	Seed        uint64
	Days        int
	WarmupDays  int
	StartMillis int64
	Salt        string
	Margin      float64 // legitimate volume above Events before trimming
}

// ConfigFor returns the configuration for SIZE=demo|eval.
func ConfigFor(size string, seed uint64) (Config, error) {
	cfg := Config{Size: size, Seed: seed, Days: 24, WarmupDays: 4, Salt: "synthetic-dev-salt", Margin: 1.06,
		StartMillis: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()}
	switch size {
	case "demo":
		cfg.Events, cfg.Accounts = 200_000, 3_000
	case "eval":
		cfg.Events, cfg.Accounts = 2_000_000, 30_000
	case "parity":
		cfg.Events, cfg.Accounts, cfg.Days, cfg.WarmupDays, cfg.Margin = 5_000, 60, 6, 1, 1.25
	default:
		return cfg, fmt.Errorf("unknown size %q (demo|eval|parity)", size)
	}
	return cfg, nil
}

// Label is the ground truth for one transaction.
type Label struct {
	TransactionID string
	AccountID     string
	TSMillis      int64
	Fraud         bool
	Pattern       string
	Split         string
}

// Dataset is the generated output, sorted by event time.
type Dataset struct {
	Config Config
	Events []events.Event
	Labels []Label
}

type device struct {
	id      string
	boundAt int64
}

type account struct {
	idx        int
	id         string
	country    country
	homeIPs    []string
	devices    []device
	median     float64
	txRate     float64
	loginRate  float64
	favourites []int
	travelFrom int64
	travelTo   int64
	travelTo2  string
	travelIP   string
	used       bool
}

type genEvent struct {
	ev        events.Event
	acc       int32
	seq       int32
	protected bool
	fraud     bool
	pattern   string
}

type generator struct {
	cfg      Config
	r        *rand.Rand
	catalog  []merchant
	byCat    [][]int
	catW     []float64
	accounts []*account
	evs      []genEvent
	devN     int
	seq      int32
	start    int64
	end      int64
	live     int64 // first post-warm-up millisecond
}

// Generate builds a deterministic dataset for cfg.
func Generate(cfg Config) (*Dataset, error) {
	g := &generator{cfg: cfg, r: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)), start: cfg.StartMillis}
	g.end = g.start + int64(cfg.Days)*day
	g.live = g.start + int64(cfg.WarmupDays)*day
	g.catalog = buildCatalog(g.r, max(200, cfg.Accounts/2))
	g.byCat = make([][]int, len(categories))
	for i, m := range g.catalog {
		g.byCat[m.Cat] = append(g.byCat[m.Cat], i)
	}
	for _, c := range categories {
		g.catW = append(g.catW, c.Weight)
	}
	g.makeAccounts()
	g.legit()
	if err := g.fraud(); err != nil {
		return nil, err
	}
	if err := g.trim(); err != nil {
		return nil, err
	}
	return g.finish(), nil
}

func (g *generator) newDevice() string {
	g.devN++
	sum := sha256.Sum256([]byte(g.cfg.Salt + ":dev:" + strconv.Itoa(g.devN)))
	return hex.EncodeToString(sum[:])
}

func (g *generator) ipPrefix() string {
	return fmt.Sprintf("%d.%d.%d.0/24", 11+g.r.IntN(212), g.r.IntN(256), g.r.IntN(256))
}

func (g *generator) uuidv7(ms int64) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(ms)<<16) // #nosec G115 -- synthetic positive timestamps
	binary.BigEndian.PutUint64(b[8:16], g.r.Uint64())
	r2 := g.r.Uint32()
	b[6] = 0x70 | byte(r2&0x0f)
	b[7] = byte(r2 >> 8)
	b[8] = 0x80 | (b[8] & 0x3f)
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

func pickWeighted(r *rand.Rand, w []float64) int {
	var total float64
	for _, x := range w {
		total += x
	}
	x := r.Float64() * total
	for i, v := range w {
		x -= v
		if x < 0 {
			return i
		}
	}
	return len(w) - 1
}

func (g *generator) poisson(mean float64) int {
	if mean > 30 {
		return max(0, int(math.Round(mean+math.Sqrt(mean)*g.r.NormFloat64())))
	}
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= g.r.Float64()
		if p < l {
			return k
		}
		k++
	}
}

var diurnal = []float64{1, 0.5, 0.3, 0.3, 0.3, 0.5, 1.5, 3, 4, 4.5, 5, 5.5, 6, 5.5, 5, 5, 5.5, 6, 6.5, 6, 5, 4, 3, 2}

func (g *generator) randomTime(from, to int64) int64 {
	if to <= from {
		return from
	}
	// Day uniform, hour diurnal, rest uniform; resample if outside [from, to).
	for i := 0; i < 20; i++ {
		dayStart := g.start + int64(g.r.IntN(g.cfg.Days))*day
		ts := dayStart + int64(pickWeighted(g.r, diurnal))*hour + g.r.Int64N(hour)
		if ts >= from && ts < to {
			return ts
		}
	}
	return from + g.r.Int64N(to-from)
}

func (g *generator) makeAccounts() {
	cw := make([]float64, len(countries))
	for i, c := range countries {
		cw[i] = c.Weight
	}
	var expected float64
	for i := 0; i < g.cfg.Accounts; i++ {
		a := &account{idx: i, id: fmt.Sprintf("acc_%08d", i*7919%100_000_000), country: countries[pickWeighted(g.r, cw)]}
		a.homeIPs = []string{g.ipPrefix()}
		if g.r.Float64() < 0.4 {
			a.homeIPs = append(a.homeIPs, g.ipPrefix())
		}
		a.median = 2500 * math.Exp(0.55*g.r.NormFloat64())
		a.txRate = 2.0 * math.Exp(0.45*g.r.NormFloat64()-0.1)
		a.loginRate = 0.9 * math.Exp(0.4*g.r.NormFloat64()-0.08)
		for k := 0; k < 6+g.r.IntN(7); k++ {
			a.favourites = append(a.favourites, g.randomMerchant(true))
		}
		if g.r.Float64() < 0.03 {
			a.travelFrom = g.randomTime(g.live, g.end-4*day)
			a.travelTo = a.travelFrom + int64(2+g.r.IntN(3))*day
			a.travelTo2 = otherCountries[g.r.IntN(len(otherCountries))]
			a.travelIP = g.ipPrefix()
		}
		expected += (a.txRate + a.loginRate) * float64(g.cfg.Days)
		g.accounts = append(g.accounts, a)
	}
	// Scale rates so legitimate volume is about Margin above the target; trim() removes the excess.
	scale := g.cfg.Margin * float64(g.cfg.Events) / (expected + 2.4*float64(g.cfg.Accounts))
	for _, a := range g.accounts {
		a.txRate *= scale
		a.loginRate *= scale
	}
}

// randomMerchant picks a merchant by spend weight; favourites never include betting or P2P.
func (g *generator) randomMerchant(favourite bool) int {
	for {
		ci := pickWeighted(g.r, g.catW)
		mcc := categories[ci].MCC
		if favourite && (mcc == "7995" || mcc == "4829") {
			continue
		}
		list := g.byCat[ci]
		return list[g.r.IntN(len(list))]
	}
}

func (g *generator) merchantOfMCC(mccs ...string) int {
	ci := catIndex(mccs[g.r.IntN(len(mccs))])
	list := g.byCat[ci]
	return list[g.r.IntN(len(list))]
}

func (g *generator) add(acc *account, e events.Event, protected bool, pattern string) {
	g.seq++
	e.AccountID = acc.id
	e.EventID = g.uuidv7(e.TSMillis)
	g.evs = append(g.evs, genEvent{ev: e, acc: int32(acc.idx), seq: g.seq, protected: protected, // #nosec G115 -- bounded by account count
		fraud: pattern != "" && e.Kind == events.KindTransaction && pattern != PatternNone, pattern: pattern})
}

func (a *account) deviceAt(r *rand.Rand, ts int64, primary bool) string {
	if primary || len(a.devices) == 1 {
		return a.devices[0].id
	}
	var avail []string
	for _, d := range a.devices {
		if d.boundAt < ts {
			avail = append(avail, d.id)
		}
	}
	if len(avail) == 0 {
		return a.devices[0].id
	}
	return avail[r.IntN(len(avail))]
}

func (g *generator) bind(acc *account, ts int64, protected bool, pattern string) string {
	id := g.newDevice()
	acc.devices = append(acc.devices, device{id: id, boundAt: ts})
	g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: ts, DeviceID: id, Change: events.ChangeDeviceBound}, protected, pattern)
	return id
}

func (g *generator) legitTx(acc *account, ts int64, merchantIdx int, amountMul float64, deviceOverride string) {
	m := g.catalog[merchantIdx]
	cat := categories[m.Cat]
	channel := cat.Channel
	if cat.AltChannel != "" && g.r.Float64() < 0.3 {
		channel = cat.AltChannel
	}
	amount := acc.median * cat.Factor * amountMul * math.Exp(0.6*g.r.NormFloat64())
	amt := min(int64(3_000_000), max(int64(50), int64(math.Round(amount))))
	country, ip := acc.country.Code, acc.homeIPs[g.r.IntN(len(acc.homeIPs))]
	if acc.travelTo > 0 && ts >= acc.travelFrom && ts < acc.travelTo {
		country, ip = acc.travelTo2, acc.travelIP
		if channel == events.ChannelEcommerce && g.r.Float64() < 0.5 {
			channel = events.ChannelCardPresent
		}
	}
	dev := deviceOverride
	if dev == "" {
		dev = acc.deviceAt(g.r, ts, channel == events.ChannelCardPresent)
	}
	g.add(acc, events.Event{Kind: events.KindTransaction, TSMillis: ts, DeviceID: dev, IPPrefix: ip, Country: country,
		AmountMinor: amt, Currency: acc.country.Currency, MerchantID: m.ID, MerchantName: m.Name, MCC: cat.MCC, Channel: channel}, false, PatternNone)
}

var loginMethods = []string{"password", "otp", "biometric"}

func (g *generator) legit() {
	for _, acc := range g.accounts {
		// Devices bound during warm-up (protected so new_device stays meaningful).
		g.bind(acc, g.start+g.r.Int64N(12*hour), true, "")
		if g.r.Float64() < 0.3 {
			g.bind(acc, g.start+12*hour+g.r.Int64N(int64(g.cfg.WarmupDays)*day-12*hour), true, "")
		}
		// Legitimate credential changes and device upgrades.
		if g.r.Float64() < 0.04 {
			ts := g.randomTime(g.live, g.end)
			g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: ts, DeviceID: acc.devices[0].id, Change: events.ChangePasswordChanged}, true, "")
			if g.r.Float64() < 0.5 { // phone upgrade right after a reset: a legitimate R001 trigger
				nd := g.bind(acc, ts+5*minute+g.r.Int64N(30*minute), true, "")
				g.legitTx(acc, ts+40*minute+g.r.Int64N(15*minute), acc.favourites[g.r.IntN(len(acc.favourites))], 1, nd)
			}
		}
		if g.r.Float64() < 0.01 {
			g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: g.randomTime(g.live, g.end), DeviceID: acc.devices[0].id, Change: events.ChangeEmailChanged}, true, "")
		}
		if g.r.Float64() < 0.02 {
			g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: g.randomTime(g.live, g.end), DeviceID: acc.devices[0].id, Change: events.ChangePhoneChanged}, true, "")
		}
		if g.r.Float64() < 0.02 {
			g.bind(acc, g.randomTime(g.live, g.end), true, "")
		}
		// Transactions.
		n := g.poisson(acc.txRate * float64(g.cfg.Days))
		for k := 0; k < n; k++ {
			ts := g.randomTime(g.start, g.end)
			mi := acc.favourites[g.r.IntN(len(acc.favourites))]
			if g.r.Float64() < 0.2 {
				mi = g.randomMerchant(false)
			}
			g.legitTx(acc, ts, mi, 1, "")
		}
		// Occasional rent-like P2P transfer and transit bursts.
		if g.r.Float64() < 0.05 {
			g.legitTx(acc, g.randomTime(g.start, g.end), g.merchantOfMCC("4829"), 4, "")
		}
		if g.r.Float64() < 0.003 {
			ts := g.randomTime(g.live, g.end-hour)
			mi := g.merchantOfMCC("4111")
			for k := 0; k < 6+g.r.IntN(6); k++ {
				ts += 50_000 + g.r.Int64N(70_000)
				g.legitTx(acc, ts, mi, 1, "")
			}
		}
		// Logins.
		n = g.poisson(acc.loginRate * float64(g.cfg.Days))
		for k := 0; k < n; k++ {
			ts := g.randomTime(g.start, g.end)
			g.add(acc, events.Event{Kind: events.KindLogin, TSMillis: ts, DeviceID: acc.deviceAt(g.r, ts, false), IPPrefix: acc.homeIPs[0],
				Country: acc.country.Code, Success: g.r.Float64() < 0.97, Method: loginMethods[pickWeighted(g.r, []float64{5, 2, 3})]}, false, PatternNone)
		}
	}
}

func (g *generator) pickAccount() *account {
	for i := 0; i < 8; i++ {
		a := g.accounts[g.r.IntN(len(g.accounts))]
		if !a.used {
			a.used = true
			return a
		}
	}
	return g.accounts[g.r.IntN(len(g.accounts))]
}

func (g *generator) incidentStart() int64 { return g.live + g.r.Int64N(g.end-g.live-3*hour) }

func (g *generator) fraudTx(acc *account, ts int64, mi int, amount int64, channel, dev, ip, country, pattern string) {
	m := g.catalog[mi]
	g.add(acc, events.Event{Kind: events.KindTransaction, TSMillis: ts, DeviceID: dev, IPPrefix: ip, Country: country,
		AmountMinor: min(int64(10_000_000), max(int64(50), amount)), Currency: acc.country.Currency, MerchantID: m.ID,
		MerchantName: m.Name, MCC: categories[m.Cat].MCC, Channel: channel}, true, pattern)
}

func (g *generator) attackerCountry(acc *account) string {
	if g.r.Float64() < 0.5 {
		return acc.country.Code
	}
	return otherCountries[g.r.IntN(len(otherCountries))]
}

func (g *generator) incidentATO() int {
	acc, t := g.pickAccount(), g.incidentStart()
	dev, ip, ctry := g.newDevice(), g.ipPrefix(), g.attackerCountry(acc)
	for k := 0; k < g.r.IntN(3); k++ {
		g.add(acc, events.Event{Kind: events.KindLogin, TSMillis: t - int64(k+1)*(2*minute+g.r.Int64N(5*minute)), DeviceID: dev, IPPrefix: ip, Country: ctry, Success: false, Method: "password"}, true, PatternATO)
	}
	change := events.ChangeEmailChanged
	if g.r.Float64() < 0.4 {
		change = events.ChangePasswordChanged
	}
	g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: t, DeviceID: dev, Change: change}, true, PatternATO)
	t += minute + g.r.Int64N(4*minute)
	g.add(acc, events.Event{Kind: events.KindLogin, TSMillis: t, DeviceID: dev, IPPrefix: ip, Country: ctry, Success: true, Method: "password"}, true, PatternATO)
	if g.r.Float64() < 0.2 {
		t += minute
		g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: t, DeviceID: dev, Change: events.ChangeDeviceBound}, true, PatternATO)
	}
	n := 1 + g.r.IntN(4)
	for k := 0; k < n; k++ {
		t += minute + g.r.Int64N(12*minute)
		amount := int64(acc.median * (8 + 22*g.r.Float64()))
		g.fraudTx(acc, t, g.merchantOfMCC("5732", "5944", "5815", "5311"), amount, events.ChannelEcommerce, dev, ip, ctry, PatternATO)
	}
	return n
}

func (g *generator) incidentCardTesting() int {
	acc, t := g.pickAccount(), g.incidentStart()
	dev, ip, ctry := g.newDevice(), g.ipPrefix(), g.attackerCountry(acc)
	n := 8 + g.r.IntN(18)
	for k := 0; k < n; k++ {
		t += 10_000 + g.r.Int64N(40_000)
		g.fraudTx(acc, t, g.merchantOfMCC("5815", "5999", "4816"), 50+g.r.Int64N(251), events.ChannelEcommerce, dev, ip, ctry, PatternCardTesting)
	}
	if g.r.Float64() < 0.5 {
		t += 2*minute + g.r.Int64N(18*minute)
		g.fraudTx(acc, t, g.merchantOfMCC("5732", "5311", "5399"), 20_000+g.r.Int64N(130_000), events.ChannelEcommerce, dev, ip, ctry, PatternCardTesting)
		n++
	}
	return n
}

func (g *generator) incidentMule() int {
	acc, t := g.pickAccount(), g.incidentStart()
	devs := []string{}
	for k := 0; k < 2+g.r.IntN(3); k++ {
		t += 3*minute + g.r.Int64N(20*minute)
		id := g.newDevice()
		devs = append(devs, id)
		g.add(acc, events.Event{Kind: events.KindDevice, TSMillis: t, DeviceID: id, Change: events.ChangeDeviceBound}, true, PatternMule)
		g.add(acc, events.Event{Kind: events.KindLogin, TSMillis: t + 30_000, DeviceID: id, IPPrefix: g.ipPrefix(), Country: acc.country.Code, Success: true, Method: "otp"}, true, PatternMule)
	}
	n := 4 + g.r.IntN(7)
	for k := 0; k < n; k++ {
		t += 2*minute + g.r.Int64N(18*minute)
		amount := (50 + g.r.Int64N(450)) * 1000
		g.fraudTx(acc, t, g.merchantOfMCC("4829"), amount, events.ChannelP2P, devs[g.r.IntN(len(devs))], g.ipPrefix(), acc.country.Code, PatternMule)
	}
	return n
}

func (g *generator) incidentFriendly() int {
	acc, t := g.pickAccount(), g.incidentStart()
	amount := int64(acc.median * (1.5 + 2.5*g.r.Float64()))
	g.fraudTx(acc, t, g.merchantOfMCC("5815", "5816", "5399"), amount, events.ChannelEcommerce, acc.devices[0].id, acc.homeIPs[0], acc.country.Code, PatternFriendly)
	return 1
}

// fraud injects incidents until fraudulent transactions reach FraudRate of the post-trim total.
func (g *generator) fraud() error {
	var legitTx, legitDroppable, fixed int
	for _, e := range g.evs {
		if !e.protected {
			legitDroppable++
			if e.ev.Kind == events.KindTransaction {
				legitTx++
			}
		} else {
			fixed++
		}
	}
	perPattern := map[string]int{}
	fraudTx, fraudEvents := 0, 0
	for iter := 0; iter < 1_000_000; iter++ {
		total := len(g.evs)
		drop := total - g.cfg.Events
		dropTx := int(math.Round(float64(drop) * float64(legitTx) / float64(legitDroppable)))
		keptTx := legitTx - dropTx
		if fraudTx > 0 && float64(fraudTx) >= FraudRate*float64(fraudTx+keptTx) {
			_ = fraudEvents
			return nil
		}
		// Choose the pattern furthest below its target share.
		best, bestGap := 0, math.Inf(-1)
		for i, p := range patternShare {
			gap := p.share*float64(fraudTx+1) - float64(perPattern[p.name])
			if gap > bestGap {
				best, bestGap = i, gap
			}
		}
		before := len(g.evs)
		var n int
		switch patternShare[best].name {
		case PatternATO:
			n = g.incidentATO()
		case PatternCardTesting:
			n = g.incidentCardTesting()
		case PatternMule:
			n = g.incidentMule()
		default:
			n = g.incidentFriendly()
		}
		perPattern[patternShare[best].name] += n
		fraudTx += n
		fraudEvents += len(g.evs) - before
	}
	return fmt.Errorf("fraud injection did not converge")
}

// trim drops random unprotected legitimate events (stratified by kind) to reach exactly cfg.Events.
func (g *generator) trim() error {
	drop := len(g.evs) - g.cfg.Events
	if drop < 0 {
		return fmt.Errorf("generated %d events, fewer than the target %d", len(g.evs), g.cfg.Events)
	}
	var txIdx, otherIdx []int
	for i, e := range g.evs {
		if e.protected {
			continue
		}
		if e.ev.Kind == events.KindTransaction {
			txIdx = append(txIdx, i)
		} else {
			otherIdx = append(otherIdx, i)
		}
	}
	dropTx := int(math.Round(float64(drop) * float64(len(txIdx)) / float64(len(txIdx)+len(otherIdx))))
	dropOther := drop - dropTx
	if dropTx > len(txIdx) || dropOther > len(otherIdx) {
		return fmt.Errorf("not enough droppable events")
	}
	remove := make([]bool, len(g.evs))
	for _, sel := range []struct {
		idx []int
		n   int
	}{{txIdx, dropTx}, {otherIdx, dropOther}} {
		for k := 0; k < sel.n; k++ { // partial Fisher-Yates
			j := k + g.r.IntN(len(sel.idx)-k)
			sel.idx[k], sel.idx[j] = sel.idx[j], sel.idx[k]
			remove[sel.idx[k]] = true
		}
	}
	kept := g.evs[:0]
	for i, e := range g.evs {
		if !remove[i] {
			kept = append(kept, e)
		}
	}
	g.evs = kept
	return nil
}

func (g *generator) split(ts int64) string {
	d := int((ts - g.start) / day)
	live := g.cfg.Days - g.cfg.WarmupDays
	trainEnd := g.cfg.WarmupDays + int(math.Round(0.70*float64(live)))
	validEnd := trainEnd + int(math.Round(0.15*float64(live)))
	switch {
	case d < g.cfg.WarmupDays:
		return "warmup"
	case d < trainEnd:
		return "train"
	case d < validEnd:
		return "valid"
	default:
		return "test"
	}
}

func (g *generator) finish() *Dataset {
	// Per account: strictly increasing timestamps (features read strictly before as_of).
	slices.SortFunc(g.evs, func(a, b genEvent) int {
		if a.acc != b.acc {
			return int(a.acc - b.acc)
		}
		if a.ev.TSMillis != b.ev.TSMillis {
			if a.ev.TSMillis < b.ev.TSMillis {
				return -1
			}
			return 1
		}
		return int(a.seq - b.seq)
	})
	for i := 1; i < len(g.evs); i++ {
		if g.evs[i].acc == g.evs[i-1].acc && g.evs[i].ev.TSMillis <= g.evs[i-1].ev.TSMillis {
			g.evs[i].ev.TSMillis = g.evs[i-1].ev.TSMillis + 1
		}
	}
	slices.SortFunc(g.evs, func(a, b genEvent) int {
		if a.ev.TSMillis != b.ev.TSMillis {
			if a.ev.TSMillis < b.ev.TSMillis {
				return -1
			}
			return 1
		}
		if a.acc != b.acc {
			return int(a.acc - b.acc)
		}
		return int(a.seq - b.seq)
	})
	ds := &Dataset{Config: g.cfg, Events: make([]events.Event, len(g.evs))}
	for i := range g.evs {
		e := &g.evs[i]
		ds.Events[i] = e.ev
		if e.ev.Kind == events.KindTransaction {
			p := e.pattern
			if p == "" || !e.fraud {
				p = PatternNone
			}
			ds.Labels = append(ds.Labels, Label{TransactionID: e.ev.EventID, AccountID: e.ev.AccountID, TSMillis: e.ev.TSMillis,
				Fraud: e.fraud, Pattern: p, Split: g.split(e.ev.TSMillis)})
		}
	}
	return ds
}

// WriteEvents writes events.jsonl.
func (d *Dataset) WriteEvents(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	buf := make([]byte, 0, 512)
	for i := range d.Events {
		buf = d.Events[i].AppendJSON(buf[:0])
		buf = append(buf, '\n')
		if _, err := bw.Write(buf); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// WriteLabels writes labels.csv.
func (d *Dataset) WriteLabels(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	if _, err := bw.WriteString("transaction_id,account_id,event_ts,is_fraud,pattern,split\n"); err != nil {
		return err
	}
	for _, l := range d.Labels {
		fraud := "0"
		if l.Fraud {
			fraud = "1"
		}
		if _, err := fmt.Fprintf(bw, "%s,%s,%s,%s,%s,%s\n", l.TransactionID, l.AccountID, events.FormatTS(l.TSMillis), fraud, l.Pattern, l.Split); err != nil {
			return err
		}
	}
	return bw.Flush()
}
