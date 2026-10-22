// Package api implements the gRPC services (Scorer, CaseService), interceptors and the HTTP gateway.
package api

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// CurrencyAllowlist matches schemas/events/transaction.json.
var CurrencyAllowlist = map[string]bool{
	"USD": true, "EUR": true, "GBP": true, "CAD": true, "AUD": true, "JPY": true, "CHF": true, "SEK": true,
	"NOK": true, "DKK": true, "PLN": true, "MXN": true, "BRL": true, "SGD": true, "NZD": true,
}

// MCCAllowlist matches schemas/events/transaction.json: the merchant categories of the synthetic catalog
// (cmd/gen), the only ones the model and rules were built on.
var MCCAllowlist = map[string]bool{
	"4111": true, "4511": true, "4816": true, "4829": true, "5311": true, "5399": true, "5411": true, "5541": true, "5651": true, "5732": true, "5812": true, "5814": true, "5815": true, "5816": true, "5912": true, "5944": true, "5999": true, "7011": true, "7995": true,
}

var (
	reUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reAccount = regexp.MustCompile(`^acc_[0-9a-z]{6,32}$`)
	reDevice  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reIP24    = regexp.MustCompile(`^(25[0-5]|2[0-4]\d|1?\d?\d)\.(25[0-5]|2[0-4]\d|1?\d?\d)\.(25[0-5]|2[0-4]\d|1?\d?\d)\.0/24$`)
	reCountry = regexp.MustCompile(`^[A-Z]{2}$`)
	reMerch   = regexp.MustCompile(`^m_[0-9a-z]{4,32}$`)
)

// ValidateTransaction returns field violations in field order (empty when valid).
func ValidateTransaction(tx *fraudv1.Transaction) []*errdetails.BadRequest_FieldViolation {
	var v []*errdetails.BadRequest_FieldViolation
	add := func(field, desc string) {
		v = append(v, &errdetails.BadRequest_FieldViolation{Field: "transaction." + field, Description: desc})
	}
	if tx == nil {
		return []*errdetails.BadRequest_FieldViolation{{Field: "transaction", Description: "required"}}
	}
	if !reUUID.MatchString(tx.TransactionId) {
		add("transaction_id", "must be a lowercase UUID (UUIDv7 recommended)")
	}
	if !reAccount.MatchString(tx.AccountId) {
		add("account_id", "must be a synthetic account token acc_[0-9a-z]{6,32}")
	}
	if tx.AmountMinor < 1 || tx.AmountMinor > MaxAmountMinor {
		add("amount_minor", "must be between 1 and 10000000")
	}
	if !CurrencyAllowlist[tx.Currency] {
		add("currency", "not in the ISO 4217 allowlist")
	}
	if tx.MerchantId != "" && !reMerch.MatchString(tx.MerchantId) {
		add("merchant_id", "must match m_[0-9a-z]{4,32}")
	}
	if !MCCAllowlist[tx.Mcc] {
		add("mcc", "not in the MCC allowlist")
	}
	if tx.Channel == fraudv1.Channel_CHANNEL_UNSPECIFIED || fraudv1.Channel_name[int32(tx.Channel)] == "" {
		add("channel", "must be CARD_PRESENT, ECOMMERCE or P2P")
	}
	if !reDevice.MatchString(tx.DeviceId) {
		add("device_id", "must be a salted SHA-256 hex digest")
	}
	if !reIP24.MatchString(tx.IpPrefix) {
		add("ip_prefix", "must be an IPv4 /24 prefix (a.b.c.0/24)")
	}
	if !reCountry.MatchString(tx.Country) {
		add("country", "must be ISO 3166-1 alpha-2")
	}
	if utf8.RuneCountInString(tx.MerchantName) > MaxMerchantNameRunes || strings.ContainsFunc(tx.MerchantName, isControl) {
		add("merchant_name", "at most 120 characters, no control characters")
	}
	if tx.EventTs != nil && !tx.EventTs.IsValid() {
		add("event_ts", "invalid timestamp")
	}
	return v
}

// isControl matches C0 and C1 controls, DEL and the bidirectional marks and overrides (which can make a name
// render as something else, "Shop\u202egnp.exe"); Postgres rejects NUL, and none belong in a display name.
func isControl(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func invalidArgument(v []*errdetails.BadRequest_FieldViolation) error {
	st := status.New(codes.InvalidArgument, "invalid request")
	withDetails, err := st.WithDetails(&errdetails.BadRequest{FieldViolations: v})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}
