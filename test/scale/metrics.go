//go:build scale

package scale

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// MetricsReport holds timing and throughput data for a scale test run.
type MetricsReport struct {
	T0             time.Time     // block message injected
	T1             time.Time     // first MINED callback received
	T2             time.Time     // last MINED callback received
	T3             time.Time     // all BLOCK_PROCESSED received
	TotalWallClock time.Duration // total test duration
	ServerStats    []ServerStats
}

// collectMetrics gathers timing data from the callback fleet.
func collectMetrics(fleet *CallbackFleet, t0 time.Time) *MetricsReport {
	report := &MetricsReport{
		T0:          t0,
		ServerStats: make([]ServerStats, fleet.Count()),
	}

	var earliestFirst, latestLast time.Time
	var latestBP time.Time

	for i := 0; i < fleet.Count(); i++ {
		stats := fleet.GetServer(i).Stats()
		report.ServerStats[i] = stats

		if !stats.FirstCallback.IsZero() {
			if earliestFirst.IsZero() || stats.FirstCallback.Before(earliestFirst) {
				earliestFirst = stats.FirstCallback
			}
		}
		if !stats.LastCallback.IsZero() {
			if stats.LastCallback.After(latestLast) {
				latestLast = stats.LastCallback
			}
		}

		// Check BLOCK_PROCESSED timing.
		bpPayloads := fleet.GetServer(i).BlockProcessedPayloads()
		if len(bpPayloads) > 0 {
			if stats.LastCallback.After(latestBP) {
				latestBP = stats.LastCallback
			}
		}
	}

	report.T1 = earliestFirst
	report.T2 = latestLast
	report.T3 = latestBP
	report.TotalWallClock = time.Since(t0)

	return report
}

// printReport outputs a formatted summary.
func printReport(t *testing.T, report *MetricsReport) {
	t.Helper()

	var totalMined, totalBP, totalTxids int
	var totalBytes int64
	for _, s := range report.ServerStats {
		totalMined += s.MinedCallbacks
		totalBP += s.BlockProcessed
		totalTxids += s.TotalTxids
		totalBytes += s.TotalBytes
	}

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("╔══════════════════════════════════════════════════════╗\n")
	sb.WriteString("║            SCALE TEST METRICS REPORT                ║\n")
	sb.WriteString("╠══════════════════════════════════════════════════════╣\n")
	sb.WriteString(fmt.Sprintf("║ Total wall clock:        %-27s ║\n", report.TotalWallClock.Round(time.Millisecond)))

	if !report.T1.IsZero() {
		sb.WriteString(fmt.Sprintf("║ T0→T1 (first MINED):     %-27s ║\n", report.T1.Sub(report.T0).Round(time.Millisecond)))
	}
	if !report.T2.IsZero() {
		sb.WriteString(fmt.Sprintf("║ T0→T2 (last MINED):      %-27s ║\n", report.T2.Sub(report.T0).Round(time.Millisecond)))
	}
	if !report.T3.IsZero() {
		sb.WriteString(fmt.Sprintf("║ T0→T3 (all BP):          %-27s ║\n", report.T3.Sub(report.T0).Round(time.Millisecond)))
	}

	sb.WriteString("╠══════════════════════════════════════════════════════╣\n")
	sb.WriteString(fmt.Sprintf("║ Total MINED callbacks:   %-27d ║\n", totalMined))
	sb.WriteString(fmt.Sprintf("║ Total BLOCK_PROCESSED:   %-27d ║\n", totalBP))
	sb.WriteString(fmt.Sprintf("║ Total txids delivered:   %-27d ║\n", totalTxids))
	sb.WriteString(fmt.Sprintf("║ Total bytes received:    %-27s ║\n", formatBytes(totalBytes)))
	sb.WriteString(fmt.Sprintf("║ Callback servers:        %-27d ║\n", len(report.ServerStats)))

	if !report.T2.IsZero() && report.T2.Sub(report.T0) > 0 {
		throughput := float64(totalTxids) / report.T2.Sub(report.T0).Seconds()
		sb.WriteString(fmt.Sprintf("║ Txid throughput:         %-27s ║\n", fmt.Sprintf("%.0f txids/sec", throughput)))
	}

	sb.WriteString("╠══════════════════════════════════════════════════════╣\n")
	sb.WriteString("║ Per-server summary (first 10):                      ║\n")
	sb.WriteString("║  Port   | MINED | BP | TxIDs  | Bytes              ║\n")

	showCount := 10
	if len(report.ServerStats) < showCount {
		showCount = len(report.ServerStats)
	}
	for i := 0; i < showCount; i++ {
		s := report.ServerStats[i]
		sb.WriteString(fmt.Sprintf("║  %5d  | %5d | %2d | %6d | %-18s ║\n",
			s.Port, s.MinedCallbacks, s.BlockProcessed, s.TotalTxids, formatBytes(s.TotalBytes)))
	}
	if len(report.ServerStats) > showCount {
		sb.WriteString(fmt.Sprintf("║  ... and %d more servers                             ║\n", len(report.ServerStats)-showCount))
	}

	sb.WriteString("╚══════════════════════════════════════════════════════╝\n")

	t.Log(sb.String())
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
