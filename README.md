# Copilot Usage

[![CI](https://github.com/Antupis/copilot-usage/actions/workflows/ci.yml/badge.svg)](https://github.com/Antupis/copilot-usage/actions/workflows/ci.yml)

Small terminal dashboard for GitHub Copilot quota usage. It shows:

- Progress through the current Copilot billing month.
- Premium interaction credits used during that month.
- A weekly target: the share of the quota you can have used by this week's checkpoint while staying on pace for the month.
- Projected usage at the period end and at the checkpoint if the current average rate continues.
- The quota reset date, last successful update, and next API query.

The dashboard redraws once per second, but queries GitHub only every five minutes by default.

## Requirements

- Go 1.23 or newer to build the application.
- GitHub CLI (`gh`) installed and authenticated.
- A GitHub Copilot account with quota information.
- A terminal with ANSI support. Ghostty works with the default settings.

Authenticate GitHub CLI if needed:

```sh
gh auth login
```

The app invokes `gh api /copilot_internal/user`, so it does not read, store, or print your GitHub token.

## Install

Install the latest version with Go:

```sh
go install github.com/Antupis/copilot-usage@latest
```

The resulting `copilot-usage` binary is installed into Go's binary directory. Ensure that directory is on your `PATH` before running it.

Or, from a checkout, build and install into `~/.local/bin` (override with `PREFIX`, e.g. `make install PREFIX=/usr/local`). Run `git pull && make install` to update:

```sh
make install
```

## Run

From this directory:

```sh
go run .
```

Build a standalone binary:

```sh
go build -o copilot-usage .
./copilot-usage
```

On macOS, install it into Go's binary directory with:

```sh
go install .
```

If that directory is on your `PATH`, start it with:

```sh
copilot-usage
```

To build on the Linux container for an Apple Silicon Mac:

```sh
GOOS=darwin GOARCH=arm64 go build -o copilot-usage-darwin-arm64 .
```

For an Intel Mac, use `GOARCH=amd64` instead. Run the resulting binary on the Mac where `gh auth login` has been completed.

Press `Ctrl+C` to quit. The cursor and terminal formatting are restored when the app exits.

## Options

```text
-interval duration   GitHub query interval, default 5m; minimum 1m in dashboard mode
-width number        progress bar width, default from COLUMNS or 40
-ascii               use ASCII bars instead of Unicode blocks
-once                query once, print the dashboard, and exit
-oneline             query once, print one line of percentages, and exit
```

Examples:

```sh
copilot-usage -interval 10m
copilot-usage -width 30 -ascii
copilot-usage -once
```

`-once` is useful for shell scripts and checks that do not need a continuously refreshing screen.

`-oneline` prints premium usage, month elapsed, weekly target and projected period-end usage, e.g. `U 66.2% M 81.6% W 99.9% P 81.1%` (`n/a` when unavailable), for status bars and scripts, such as a Polybar `custom/script` module (`exec = copilot-usage -oneline`, `interval = 300`).

## Development

Run the same checks used by continuous integration:

```sh
gofmt -w main.go main_test.go
go vet ./...
go test ./...
go build ./...
```

## Data and limitations

The required quota fields currently come from GitHub's undocumented `/copilot_internal/user` endpoint through GitHub CLI. GitHub can change or remove this endpoint without notice. If a refresh fails after a successful query, the dashboard keeps displaying the last valid data and reports the error before retrying at the next scheduled interval.

The month progress bar uses GitHub's quota reset timestamp. If no valid reset timestamp is returned, it estimates progress using the current UTC calendar month.

The weekly target bar shows how much of the quota you can have used by this week's checkpoint while staying on pace for the month. The checkpoint is Sunday 23:00 UTC, or 23:00 UTC on the last day of the period if that comes first. It stays fixed during the week, so weekends do not make it drift ahead of actual work. After the last day's checkpoint it shows 100%.

Projections extrapolate linearly from the average usage rate since the period started (GitHub only reports the running total). They show `n/a` during the first 24 hours of a period, when the rate is too noisy to be useful, and can exceed 100%.

## License

This project is licensed under the [MIT License](LICENSE).
