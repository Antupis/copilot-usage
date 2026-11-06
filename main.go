package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultQueryInterval = 5 * time.Minute
	minimumQueryInterval = time.Minute
	queryTimeout         = 30 * time.Second
	defaultBarWidth      = 40
	minimumBarWidth      = 5
	maximumBarWidth      = 120
)

type config struct {
	Interval time.Duration
	BarWidth int
	ASCII    bool
	Once     bool
	Oneline  bool
}

type apiResponse struct {
	Login             string `json:"login"`
	CopilotPlan       string `json:"copilot_plan"`
	QuotaResetDate    string `json:"quota_reset_date"`
	QuotaResetDateUTC string `json:"quota_reset_date_utc"`
	QuotaSnapshots    struct {
		PremiumInteractions *apiQuota `json:"premium_interactions"`
	} `json:"quota_snapshots"`
}

type apiQuota struct {
	OverageCount       float64  `json:"overage_count"`
	PercentRemaining   *float64 `json:"percent_remaining"`
	QuotaRemaining     float64  `json:"quota_remaining"`
	Unlimited          bool     `json:"unlimited"`
	HasQuota           bool     `json:"has_quota"`
	TokenBasedBilling  bool     `json:"token_based_billing"`
	CreditsUsed        float64  `json:"credits_used"`
	OverageEntitlement float64  `json:"overage_entitlement"`
	Remaining          float64  `json:"remaining"`
	Entitlement        float64  `json:"entitlement"`
}

type quota struct {
	HasQuota         bool
	Unlimited        bool
	CreditsUsed      float64
	Entitlement      float64
	PercentRemaining float64
	HasPercent       bool
}

type usageData struct {
	Login     string
	Plan      string
	ResetAt   time.Time
	Quota     quota
	FetchedAt time.Time
}

type usageFetcher interface {
	Fetch(context.Context) (usageData, error)
}

type ghFetcher struct {
	Timeout time.Duration
}

func (f ghFetcher) Fetch(ctx context.Context) (usageData, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = queryTimeout
	}

	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(requestContext, "gh", "api", "/copilot_internal/user")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return usageData{}, fmt.Errorf("GitHub API request timed out after %s", timeout)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return usageData{}, errors.New("gh was not found; install GitHub CLI and ensure it is in PATH")
		}

		detail := cleanLine(stderr.String())
		if detail != "" {
			return usageData{}, fmt.Errorf("gh api failed: %s", detail)
		}
		return usageData{}, fmt.Errorf("gh api failed: %w", err)
	}

	return parseUsageJSON(stdout.Bytes(), time.Now().UTC())
}

func parseUsageJSON(raw []byte, fetchedAt time.Time) (usageData, error) {
	var response apiResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&response); err != nil {
		return usageData{}, fmt.Errorf("invalid GitHub response: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return usageData{}, errors.New("invalid GitHub response: multiple JSON values")
		}
		return usageData{}, fmt.Errorf("invalid GitHub response: %w", err)
	}

	parsedQuota := quota{}
	if snapshot := response.QuotaSnapshots.PremiumInteractions; snapshot != nil {
		percentRemaining := 0.0
		if snapshot.PercentRemaining != nil {
			percentRemaining = *snapshot.PercentRemaining
		}
		parsedQuota = quota{
			HasQuota:         snapshot.HasQuota || snapshot.Unlimited || snapshot.Entitlement > 0 || snapshot.PercentRemaining != nil,
			Unlimited:        snapshot.Unlimited,
			CreditsUsed:      snapshot.CreditsUsed,
			Entitlement:      snapshot.Entitlement,
			PercentRemaining: percentRemaining,
			HasPercent:       snapshot.PercentRemaining != nil,
		}
	}

	return usageData{
		Login:     response.Login,
		Plan:      response.CopilotPlan,
		ResetAt:   parseResetDate(response.QuotaResetDateUTC, response.QuotaResetDate),
		Quota:     parsedQuota,
		FetchedAt: fetchedAt.UTC(),
	}, nil
}

func parseResetDate(values ...string) time.Time {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	}

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, layout := range layouts {
			parsed, err := time.Parse(layout, value)
			if err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	if stderr == nil {
		stderr = io.Discard
	}

	flags := flag.NewFlagSet("copilot-usage", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var result config
	flags.DurationVar(&result.Interval, "interval", defaultQueryInterval, "GitHub query interval (minimum 1m in dashboard mode)")
	flags.IntVar(&result.BarWidth, "width", 0, "progress bar width (default: based on COLUMNS, or 40)")
	flags.BoolVar(&result.ASCII, "ascii", false, "use ASCII progress bars instead of Unicode blocks")
	flags.BoolVar(&result.Once, "once", false, "query once, print the dashboard, and exit")
	flags.BoolVar(&result.Oneline, "oneline", false, "query once, print a single line of key percentages, and exit")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: copilot-usage [options]")
		fmt.Fprintln(stderr, "")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if result.Oneline {
		result.Once = true
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if result.Interval <= 0 {
		return config{}, errors.New("-interval must be greater than zero")
	}
	if !result.Once && result.Interval < minimumQueryInterval {
		return config{}, fmt.Errorf("-interval must be at least %s in dashboard mode", minimumQueryInterval)
	}
	if result.BarWidth != 0 && (result.BarWidth < minimumBarWidth || result.BarWidth > maximumBarWidth) {
		return config{}, fmt.Errorf("-width must be between %d and %d", minimumBarWidth, maximumBarWidth)
	}
	if result.BarWidth == 0 {
		result.BarWidth = defaultBarWidthFromEnvironment()
	}
	return result, nil
}

func defaultBarWidthFromEnvironment() int {
	columns, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS")))
	if err != nil || columns <= 0 {
		return defaultBarWidth
	}

	width := columns - 35
	if width < 10 {
		return 10
	}
	if width > 60 {
		return 60
	}
	return width
}

type appState struct {
	Data          *usageData
	LastError     error
	LastSuccessAt time.Time
	NextQueryAt   time.Time
	QueryInFlight bool
}

type fetchResult struct {
	Data        usageData
	Err         error
	AttemptedAt time.Time
	FinishedAt  time.Time
}

func runDashboard(ctx context.Context, fetcher usageFetcher, cfg config, out io.Writer) error {
	results := make(chan fetchResult, 1)
	state := appState{}

	startFetch := func() {
		if state.QueryInFlight {
			return
		}
		state.QueryInFlight = true
		attemptedAt := time.Now()
		go func() {
			data, err := fetcher.Fetch(ctx)
			finishedAt := time.Now()
			if err == nil {
				data.FetchedAt = finishedAt.UTC()
			}
			result := fetchResult{
				Data:        data,
				Err:         err,
				AttemptedAt: attemptedAt,
				FinishedAt:  finishedAt,
			}
			select {
			case results <- result:
			case <-ctx.Done():
			}
		}()
	}

	interactive := isTerminal(out)
	if interactive {
		if _, err := io.WriteString(out, "\x1b[?25l"); err != nil {
			return err
		}
		defer func() {
			_, _ = io.WriteString(out, "\x1b[?25h\x1b[0m\n")
		}()
	}

	startFetch()
	if err := writeDashboard(out, state, time.Now(), cfg, interactive); err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case result := <-results:
			state.QueryInFlight = false
			state.NextQueryAt = result.AttemptedAt.Add(cfg.Interval)
			if result.Err != nil {
				state.LastError = result.Err
			} else {
				data := result.Data
				state.Data = &data
				state.LastSuccessAt = result.FinishedAt.UTC()
				state.LastError = nil
			}
			if err := writeDashboard(out, state, time.Now(), cfg, interactive); err != nil {
				return err
			}
		case now := <-ticker.C:
			if !state.QueryInFlight && !state.NextQueryAt.IsZero() && !now.Before(state.NextQueryAt) {
				startFetch()
			}
			if err := writeDashboard(out, state, now, cfg, interactive); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func runOnce(ctx context.Context, fetcher usageFetcher, cfg config, out io.Writer) error {
	data, err := fetcher.Fetch(ctx)
	if err != nil {
		return err
	}
	if data.FetchedAt.IsZero() {
		data.FetchedAt = time.Now().UTC()
	}
	if cfg.Oneline {
		_, err := io.WriteString(out, onelineText(data, time.Now()))
		return err
	}
	state := appState{
		Data:          &data,
		LastSuccessAt: data.FetchedAt,
	}
	return writeDashboard(out, state, time.Now(), cfg, false)
}

func writeDashboard(out io.Writer, state appState, now time.Time, cfg config, clear bool) error {
	if clear {
		if _, err := io.WriteString(out, "\x1b[H\x1b[2J"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(out, dashboardText(state, now, cfg))
	return err
}

func dashboardText(state appState, now time.Time, cfg config) string {
	var output strings.Builder
	output.WriteString("GitHub Copilot Usage\n")

	if state.Data != nil {
		account := state.Data.Login
		if account == "" {
			account = "unknown"
		}
		plan := displayPlan(state.Data.Plan)
		if plan == "" {
			plan = "unknown"
		}
		fmt.Fprintf(&output, "Account: %s | Plan: %s\n", account, plan)
	} else {
		output.WriteString("Account: unknown | Plan: unknown\n")
	}
	output.WriteString("\n")

	var resetAt time.Time
	if state.Data != nil {
		resetAt = state.Data.ResetAt
	}
	monthPercent, periodStart, periodEnd, fallback := monthProgress(now, resetAt)
	if fallback {
		fmt.Fprintf(&output, "Period: %s to %s UTC (estimated)\n", periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
	} else {
		fmt.Fprintf(&output, "Period: %s to %s UTC\n", periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
	}
	fmt.Fprintf(&output, "Month elapsed  %s %5.1f%%\n", progressBar(monthPercent, cfg.BarWidth, cfg.ASCII), monthPercent*100)

	usageRaw, usageAvailable := 0.0, false
	if state.Data == nil || !state.Data.Quota.HasQuota {
		fmt.Fprintf(&output, "Premium usage  %s unavailable\n", progressBar(0, cfg.BarWidth, cfg.ASCII))
	} else if state.Data.Quota.Unlimited {
		fmt.Fprintf(&output, "Premium usage  %s unlimited\n", progressBar(0, cfg.BarWidth, cfg.ASCII))
	} else {
		usagePercent, available := usageProgress(state.Data.Quota)
		if !available {
			fmt.Fprintf(&output, "Premium usage  %s unavailable\n", progressBar(0, cfg.BarWidth, cfg.ASCII))
		} else {
			fmt.Fprintf(&output, "Premium usage  %s %5.1f%%\n", progressBar(usagePercent, cfg.BarWidth, cfg.ASCII), usagePercent*100)
			usageRaw, usageAvailable = usageRatio(state.Data.Quota)
		}
	}

	elapsed := now.UTC().Sub(periodStart)
	writeProjection := func(label string, target float64) {
		if !usageAvailable {
			fmt.Fprintf(&output, "Projected at %s: unavailable\n", label)
			return
		}
		projected, ok := projectUsage(usageRaw, monthPercent, target, elapsed)
		if !ok {
			fmt.Fprintf(&output, "Projected at %s: n/a\n", label)
			return
		}
		if entitlement := state.Data.Quota.Entitlement; entitlement > 0 {
			fmt.Fprintf(&output, "Projected at %s: %.1f%% (%s / %s credits)\n", label, projected*100, formatNumber(math.Round(projected*entitlement)), formatNumber(entitlement))
		} else {
			fmt.Fprintf(&output, "Projected at %s: %.1f%%\n", label, projected*100)
		}
	}
	writeProjection("period end", 1)

	weekPercent, checkpoint := weekBudget(now, resetAt)
	output.WriteString("\n")
	fmt.Fprintf(&output, "Weekly Checkpoint: %s UTC\n", checkpoint.Format("2006-01-02 15:04"))
	fmt.Fprintf(&output, "Weekly target  %s %5.1f%%\n", progressBar(weekPercent, cfg.BarWidth, cfg.ASCII), weekPercent*100)
	writeProjection("checkpoint", weekPercent)

	output.WriteString("\n")
	if state.Data != nil && state.Data.Quota.HasQuota && !state.Data.Quota.Unlimited && state.Data.Quota.Entitlement > 0 {
		fmt.Fprintf(&output, "%s / %s premium credits\n", formatNumber(state.Data.Quota.CreditsUsed), formatNumber(state.Data.Quota.Entitlement))
	} else if state.Data != nil && state.Data.Quota.Unlimited {
		output.WriteString("Premium credits: unlimited\n")
	} else {
		output.WriteString("Premium credits: unavailable\n")
	}

	if resetAt.IsZero() {
		output.WriteString("Resets: unavailable\n")
	} else {
		fmt.Fprintf(&output, "Resets: %s\n", formatUTC(resetAt))
	}

	if state.Data != nil && !state.Data.FetchedAt.IsZero() {
		fmt.Fprintf(&output, "Updated: %s\n", formatUTC(state.Data.FetchedAt))
	} else {
		output.WriteString("Updated: waiting for GitHub\n")
	}

	if state.LastError != nil {
		fmt.Fprintf(&output, "Warning: refresh failed: %s\n", cleanLine(state.LastError.Error()))
	}

	if state.QueryInFlight {
		output.WriteString("Next query: in progress\n")
	} else if !state.NextQueryAt.IsZero() {
		remaining := time.Until(state.NextQueryAt)
		if !now.IsZero() {
			remaining = state.NextQueryAt.Sub(now)
		}
		if remaining < 0 {
			remaining = 0
		}
		fmt.Fprintf(&output, "Next query in: %s\n", formatDuration(remaining))
	} else if cfg.Once {
		output.WriteString("Next query: disabled (one-shot)\n")
	} else {
		output.WriteString("Next query: waiting\n")
	}

	if state.Data != nil && state.LastError != nil && !state.LastSuccessAt.IsZero() {
		fmt.Fprintf(&output, "Showing last successful data from %s\n", formatUTC(state.LastSuccessAt))
	}
	if cfg.Once {
		output.WriteString("One-shot mode\n")
	} else {
		output.WriteString("Press Ctrl+C to quit\n")
	}
	return output.String()
}

// onelineText returns usage, month elapsed, weekly target and projected
// period-end usage on one line, for status bars and scripts.
func onelineText(data usageData, now time.Time) string {
	monthPercent, periodStart, _, _ := monthProgress(now, data.ResetAt)
	weekPercent, _ := weekBudget(now, data.ResetAt)
	used, projected := "n/a", "n/a"
	if ratio, ok := usageRatio(data.Quota); ok && data.Quota.HasQuota {
		used = fmt.Sprintf("%.1f%%", ratio*100)
		if value, ok := projectUsage(ratio, monthPercent, 1, now.UTC().Sub(periodStart)); ok {
			projected = fmt.Sprintf("%.1f%%", value*100)
		}
	}
	return fmt.Sprintf("U %s M %.1f%% W %.1f%% P %s\n", used, monthPercent*100, weekPercent*100, projected)
}

func monthProgress(now, resetAt time.Time) (float64, time.Time, time.Time, bool) {
	start, end, fallback := monthPeriod(now, resetAt)
	return progressBetween(now, start, end), start, end, fallback
}

func monthPeriod(now, resetAt time.Time) (time.Time, time.Time, bool) {
	now = now.UTC()
	if resetAt.IsZero() {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), true
	}

	end := resetAt.UTC()
	return end.AddDate(0, -1, 0), end, false
}

func progressBetween(now, start, end time.Time) float64 {
	if !end.After(start) {
		return 0
	}
	progress := now.UTC().Sub(start.UTC()).Seconds() / end.UTC().Sub(start.UTC()).Seconds()
	if progress < 0 {
		return 0
	}
	if progress > 1 {
		return 1
	}
	return progress
}

// weekCheckpoint returns the earlier of the next Sunday 23:00 UTC and the
// period's last day at 23:00 UTC. Once the last day's checkpoint has passed,
// it returns the period end.
func weekCheckpoint(now, start, end time.Time) time.Time {
	now, end = now.UTC(), end.UTC()
	lastDay := end.AddDate(0, 0, -1)
	monthCheckpoint := time.Date(lastDay.Year(), lastDay.Month(), lastDay.Day(), 23, 0, 0, 0, time.UTC)
	if !monthCheckpoint.After(start) || !now.Before(monthCheckpoint) {
		return end
	}

	daysUntilSunday := (7 - int(now.Weekday())) % 7
	sunday := time.Date(now.Year(), now.Month(), now.Day()+daysUntilSunday, 23, 0, 0, 0, time.UTC)
	if !now.Before(sunday) {
		sunday = sunday.AddDate(0, 0, 7)
	}
	if sunday.Before(monthCheckpoint) {
		return sunday
	}
	return monthCheckpoint
}

// weekBudget returns the share of the period elapsed at this week's checkpoint.
func weekBudget(now, resetAt time.Time) (float64, time.Time) {
	start, end, _ := monthPeriod(now, resetAt)
	checkpoint := weekCheckpoint(now, start, end)
	return progressBetween(checkpoint, start, end), checkpoint
}

// projectUsage extrapolates usage linearly from the average rate since the
// period start. It is unavailable during the first day, when the rate is noise.
func projectUsage(usage, elapsedFraction, targetFraction float64, elapsed time.Duration) (float64, bool) {
	if elapsed < 24*time.Hour || elapsedFraction <= 0 {
		return 0, false
	}
	return usage * targetFraction / elapsedFraction, true
}

func usageProgress(q quota) (float64, bool) {
	ratio, available := usageRatio(q)
	return clamp01(ratio), available
}

func usageRatio(q quota) (float64, bool) {
	if q.Unlimited {
		return 0, false
	}
	if q.Entitlement > 0 {
		return q.CreditsUsed / q.Entitlement, true
	}
	if q.HasPercent && q.PercentRemaining >= 0 && q.PercentRemaining <= 100 {
		return 1 - q.PercentRemaining/100, true
	}
	return 0, false
}

func progressBar(progress float64, width int, ascii bool) string {
	if width < 1 {
		width = 1
	}
	progress = clamp01(progress)
	filled := int(progress * float64(width))
	if filled > width {
		filled = width
	}

	full, empty := "\u2588", "\u2591"
	if ascii {
		full, empty = "#", "-"
	}
	return "[" + strings.Repeat(full, filled) + strings.Repeat(empty, width-filled) + "]"
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func formatNumber(value float64) string {
	if math.Abs(value-math.Round(value)) < 0.000001 {
		return addThousands(strconv.FormatInt(int64(math.Round(value)), 10))
	}
	return addThousands(strconv.FormatFloat(math.Round(value*10)/10, 'f', 1, 64))
}

func addThousands(value string) string {
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = value[1:]
	}
	parts := strings.SplitN(value, ".", 2)
	integer := parts[0]
	for index := len(integer) - 3; index > 0; index -= 3 {
		integer = integer[:index] + "," + integer[index:]
	}
	if len(parts) == 2 {
		return sign + integer + "." + parts[1]
	}
	return sign + integer
}

func formatDuration(duration time.Duration) string {
	seconds := int64(math.Ceil(duration.Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	remainingSeconds := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, remainingSeconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, remainingSeconds)
}

func formatUTC(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func displayPlan(plan string) string {
	plan = strings.TrimSpace(strings.ReplaceAll(plan, "_", " "))
	if plan == "" {
		return ""
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}

func cleanLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func isTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fetcher := ghFetcher{Timeout: queryTimeout}
	if cfg.Once {
		if err := runOnce(ctx, fetcher, cfg, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		return
	}
	if err := runDashboard(ctx, fetcher, cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
