// tesla charging-history — fetch the account's charging history from the Fleet
// API (GET /api/1/dx/charging/history) and optionally import it into the local
// tesla_charges table so the cost analytics operate on real sessions.
//
// Unlike `timeline` (which stitches charges from polled vehicle_states and has
// no cost data), this is authoritative server-side history with actual billed
// fees and currency — and it is available retroactively, so it backfills
// charging the local poll log never captured.
package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/printing-press-library/library/devices/tesla/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/devices/tesla/internal/store"
)

type chargingFee struct {
	FeeType      string  `json:"feeType"`
	CurrencyCode string  `json:"currencyCode"`
	UsageBase    float64 `json:"usageBase"`
	TotalDue     float64 `json:"totalDue"`
	NetDue       float64 `json:"netDue"`
	UOM          string  `json:"uom"`
}

type chargingSession struct {
	SessionID        int64         `json:"sessionId"`
	VIN              string        `json:"vin"`
	SiteLocationName string        `json:"siteLocationName"`
	ChargeStart      string        `json:"chargeStartDateTime"`
	ChargeStop       string        `json:"chargeStopDateTime"`
	CountryCode      string        `json:"countryCode"`
	Fees             []chargingFee `json:"fees"`
}

// energyKwh returns the delivered energy from the CHARGING fee measured in kwh.
func (s chargingSession) energyKwh() float64 {
	for _, f := range s.Fees {
		if strings.EqualFold(f.FeeType, "CHARGING") && strings.EqualFold(f.UOM, "kwh") {
			return f.UsageBase
		}
	}
	return 0
}

// totalCost sums the net amount due across all fees and returns it with the
// currency code (charging history is billed in the site's local currency).
func (s chargingSession) totalCost() (float64, string) {
	var total float64
	currency := ""
	for _, f := range s.Fees {
		total += f.NetDue
		if currency == "" && f.CurrencyCode != "" {
			currency = f.CurrencyCode
		}
	}
	return total, currency
}

func newChargingHistoryCmd(flags *rootFlags) *cobra.Command {
	var (
		doImport bool
		since    string
	)
	cmd := &cobra.Command{
		Use:   "charging-history [vin]",
		Short: "Fetch charging history (real sessions, energy, fees) from the Fleet API",
		Long: `Fetches the account's charging history from the Fleet API
(GET /api/1/dx/charging/history): actual charging sessions with energy, fees,
and currency, available retroactively. Unlike timeline (which stitches charges
from polled vehicle_states and has no cost), this is authoritative server-side
data and backfills charging the local poll log never captured.

With --import, each session is written to the local tesla_charges table so
'tesla cost ledger' and 'tesla cost what-if' operate on real sessions.

Note: charging history is billed in the site's local currency (see the
"currency" field); the imported cost_usd column stores that raw amount.`,
		Example:     "  tesla-pp-cli charging-history --json\n  tesla-pp-cli charging-history --since 90d --import --json",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{"verify_noop": true, "sessions": 0}, flags)
			}
			if dryRunOK(flags) {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{"dry_run": true, "args": args}, flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			vin := ""
			if len(args) > 0 {
				vin = args[0]
			} else if vin, err = firstVehicleVIN(c); err != nil {
				return err
			}

			var cutoff time.Time
			if since != "" {
				if cutoff, err = parseChargingSince(since); err != nil {
					return usageErr(err)
				}
			}

			raw, err := c.Get("/api/1/dx/charging/history", map[string]string{"vin": vin})
			if err != nil {
				return apiErr(fmt.Errorf("charging history: %w", err))
			}
			var resp struct {
				Data []chargingSession `json:"data"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("parse charging history: %w", err)
			}

			var s *store.Store
			if doImport {
				ctx := cmd.Context()
				if s, err = store.OpenWithContext(ctx, defaultDBPath("tesla-pp-cli")); err != nil {
					return err
				}
				defer s.Close()
				if err := store.EnsureTeslaSchema(ctx, s); err != nil {
					return err
				}
			}

			type sessionOut struct {
				SessionID int64   `json:"session_id"`
				Location  string  `json:"location"`
				Start     string  `json:"start"`
				Stop      string  `json:"stop"`
				Kwh       float64 `json:"kwh"`
				Cost      float64 `json:"cost"`
				Currency  string  `json:"currency"`
			}
			out := []sessionOut{}
			var totalKwh, totalCost float64
			currency := ""
			imported := 0
			for _, sess := range resp.Data {
				if !cutoff.IsZero() {
					if t, perr := time.Parse(time.RFC3339, sess.ChargeStart); perr == nil && t.Before(cutoff) {
						continue
					}
				}
				kwh := sess.energyKwh()
				cost, ccy := sess.totalCost()
				if currency == "" {
					currency = ccy
				}
				totalKwh += kwh
				totalCost += cost
				out = append(out, sessionOut{
					SessionID: sess.SessionID, Location: sess.SiteLocationName,
					Start: sess.ChargeStart, Stop: sess.ChargeStop,
					Kwh: kwh, Cost: cost, Currency: ccy,
				})
				if doImport {
					if importChargeSession(cmd, s, sess, kwh, cost) == nil {
						imported++
					}
				}
			}

			result := map[string]any{
				"vin":        vin,
				"sessions":   out,
				"count":      len(out),
				"total_kwh":  totalKwh,
				"total_cost": totalCost,
				"currency":   currency,
			}
			if doImport {
				result["imported"] = imported
				result["import_note"] = "written to local tesla_charges; run 'tesla cost ledger' to aggregate"
			}
			return printJSONFiltered(cmd.OutOrStdout(), result, flags)
		},
	}
	cmd.Flags().BoolVar(&doImport, "import", false, "Write sessions to the local tesla_charges table for cost analytics")
	cmd.Flags().StringVar(&since, "since", "", "Only sessions on/after this point: a date (2026-01-01) or relative (90d)")
	return cmd
}

// firstVehicleVIN returns the VIN of the first vehicle on the account via
// /api/1/products (no per-vehicle call, does not wake the car).
func firstVehicleVIN(c interface {
	Get(string, map[string]string) (json.RawMessage, error)
}) (string, error) {
	raw, err := c.Get("/api/1/products", nil)
	if err != nil {
		return "", fmt.Errorf("products: %w", err)
	}
	var env struct {
		Response []struct {
			VIN string `json:"vin"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("parse products: %w", err)
	}
	for _, p := range env.Response {
		if p.VIN != "" {
			return p.VIN, nil
		}
	}
	return "", usageErr(fmt.Errorf("no vehicle found on account; pass a VIN explicitly"))
}

func importChargeSession(cmd *cobra.Command, s *store.Store, sess chargingSession, kwh, cost float64) error {
	var perKwh interface{}
	if kwh > 0 {
		perKwh = cost / kwh
	}
	rawJSON, _ := json.Marshal(sess)
	_, err := s.DB().ExecContext(cmd.Context(), `INSERT OR REPLACE INTO tesla_charges (
        vin, started_at, ended_at, fast_charger_type, location_label,
        energy_added_kwh, cost_usd, cost_per_kwh, raw_json
      ) VALUES (?,?,?,?,?,?,?,?,?)`,
		// "Tesla" is the fast_charger_type value cost ledger treats as a
		// Supercharger session (see tesla_cost.go); charging-history sessions
		// are Supercharger-network sessions.
		sess.VIN, sess.ChargeStart, sess.ChargeStop, "Tesla", sess.SiteLocationName,
		kwh, cost, perKwh, string(rawJSON))
	return err
}

// parseChargingSince accepts "Nd" (days back) or a YYYY-MM-DD / RFC3339 date.
func parseChargingSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (use e.g. 90d or 2026-01-01)", s)
}
