package cli

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChargingSessionHelpers(t *testing.T) {
	raw := `{"sessionId":1,"vin":"V","siteLocationName":"Loc","chargeStartDateTime":"2026-05-24T15:29:23+02:00","chargeStopDateTime":"2026-05-24T16:30:53+02:00","fees":[{"feeType":"CHARGING","currencyCode":"CZK","usageBase":65.9167,"netDue":572,"totalDue":692.12,"uom":"kwh"},{"feeType":"CONGESTION","currencyCode":"CZK","usageBase":0,"netDue":0,"uom":"min"}]}`
	var s chargingSession
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := s.energyKwh(); got != 65.9167 {
		t.Errorf("energyKwh = %v, want 65.9167", got)
	}
	cost, ccy := s.totalCost()
	if cost != 572 || ccy != "CZK" {
		t.Errorf("totalCost = (%v, %q), want (572, CZK)", cost, ccy)
	}
}

func TestParseChargingSince(t *testing.T) {
	t.Run("relative days", func(t *testing.T) {
		got, err := parseChargingSince("90d")
		if err != nil {
			t.Fatalf("parseChargingSince: %v", err)
		}
		want := time.Now().AddDate(0, 0, -90)
		if d := got.Sub(want); d > time.Minute || d < -time.Minute {
			t.Errorf("90d resolved to %v, expected ~%v", got, want)
		}
	})
	t.Run("absolute date", func(t *testing.T) {
		got, err := parseChargingSince("2026-01-01")
		if err != nil {
			t.Fatalf("parseChargingSince: %v", err)
		}
		if got.Year() != 2026 || got.Month() != time.January || got.Day() != 1 {
			t.Errorf("got %v, want 2026-01-01", got)
		}
	})
	t.Run("invalid input errors", func(t *testing.T) {
		if _, err := parseChargingSince("garbage"); err == nil {
			t.Error("expected error for invalid --since")
		}
	})
}
