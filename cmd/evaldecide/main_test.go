package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const caseTx = `"transaction":{"transaction_id":"0192f000-0000-4000-8000-000000000001","account_id":"acc_00000001",` +
	`"amount_minor":%AMOUNT%,"currency":"USD","merchant_id":"m_00001","merchant_name":"Shop","mcc":"5732",` +
	`"channel":"ECOMMERCE","country":"US","ip_prefix":"23.1.2.0/24",` +
	`"device_id":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","event_ts":"2026-09-21T10:00:00.000Z"}`

const feats = `{"tx_count_10m":0,"tx_count_1h":1,"tx_count_24h":3,"amount_sum_1h_minor":900,"amount_minor":9000,` +
	`"distinct_devices_24h":2,"new_device":1,"minutes_since_pwd_change":12,"minutes_since_email_change":43200,` +
	`"mcc_novel_30d":1,"channel":2,"hour_of_day_utc":10,"amount_ratio_24h":2.5}`

func row(id, amount, features, model string) string {
	tx := strings.ReplaceAll(caseTx, "%AMOUNT%", amount)
	return `{"id":"` + id + `","split":"test","input":{` + tx + `,"features":` + features + `,"model":` + model + `}}`
}

func TestDecidesEachCaseThroughTheRealScorer(t *testing.T) {
	in := strings.Join([]string{
		row("rule", "9000", feats, `{"score":0.1}`),
		row("model-down", "25000", feats, `{"unavailable":true}`),
		row("redis-down", "4000", "null", `{"score":0.9}`),
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := run(strings.NewReader(in), &out, "../../rules", "../../features/spec.yaml"); err != nil {
		t.Fatal(err)
	}
	got := map[string]Output{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var o Output
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		got[o.ID] = o
	}
	want := map[string]Output{
		"rule":       {ID: "rule", Action: "REVIEW", ReasonCodes: []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE"}},
		"model-down": {ID: "model-down", Action: "REVIEW", ReasonCodes: []string{"MODEL_UNAVAILABLE", "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE"}, Degraded: true},
		"redis-down": {ID: "redis-down", Action: "APPROVE", ReasonCodes: []string{"FEATURES_UNAVAILABLE"}, Degraded: true},
	}
	for id, w := range want {
		g := got[id]
		if g.Action != w.Action || strings.Join(g.ReasonCodes, ",") != strings.Join(w.ReasonCodes, ",") || g.Degraded != w.Degraded || g.Error != "" {
			t.Errorf("%s: got %+v, want %+v", id, g, w)
		}
		if g.RulesVersion != "2026.10.2" {
			t.Errorf("%s: rules version %q", id, g.RulesVersion)
		}
	}
}

func TestBadRowsAreReportedNotFatal(t *testing.T) {
	var out bytes.Buffer
	in := "{not json\n" + row("ok", "100", feats, `{"score":0.1}`) + "\n"
	if err := run(strings.NewReader(in), &out, "../../rules", "../../features/spec.yaml"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"error"`) || strings.Contains(lines[1], `"error"`) {
		t.Fatalf("output %q", out.String())
	}
}
