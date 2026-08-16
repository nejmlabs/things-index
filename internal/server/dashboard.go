package server

import (
	"context"
	"html/template"
	"net/http"
	"time"

	"github.com/nejmlabs/things-index/internal/queue"
)

const dashboardUsername = "things-index"

type recentJobReader interface {
	ListRecent(context.Context, int) ([]queue.Job, error)
}

type dashboardIndicator struct {
	Symbol string
	Label  string
	Class  string
}

type dashboardRow struct {
	CreatedAtISO string
	CreatedAt    string
	Title        string
	Destination  string
	Sent         dashboardIndicator
	Confirmed    dashboardIndicator
	Attempts     int
	LastError    string
	Warnings     []string
	ThingsID     string
}

type dashboardData struct {
	Rows        []dashboardRow
	GeneratedAt string
}

func newDashboardHandler(store recentJobReader, limit int) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		jobs, err := store.ListRecent(request.Context(), limit)
		if err != nil {
			http.Error(response, "load capture status", http.StatusInternalServerError)
			return
		}
		now := time.Now().UTC()
		data := dashboardData{
			Rows:        make([]dashboardRow, 0, len(jobs)),
			GeneratedAt: now.Format("02 Jan 2006 15:04:05 UTC"),
		}
		for _, job := range jobs {
			sent, confirmed := dashboardIndicators(job, now)
			createdAt := job.CreatedAt.UTC()
			data.Rows = append(data.Rows, dashboardRow{
				CreatedAtISO: createdAt.Format(time.RFC3339),
				CreatedAt:    createdAt.Format("02 Jan 2006 15:04:05 UTC"),
				Title:        job.Task.Title,
				Destination:  dashboardDestination(job),
				Sent:         sent,
				Confirmed:    confirmed,
				Attempts:     job.Attempts,
				LastError:    job.LastError,
				Warnings:     job.Warnings,
				ThingsID:     job.ThingsID,
			})
		}

		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		response.Header().Set("X-Frame-Options", "DENY")
		if err := dashboardTemplate.Execute(response, data); err != nil {
			return
		}
	})
}

func dashboardBasicAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != dashboardUsername || !secureEqual(password, token) {
			response.Header().Set("WWW-Authenticate", `Basic realm="ThingsIndex status", charset="UTF-8"`)
			http.Error(response, "unauthorised", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func dashboardIndicators(job queue.Job, now time.Time) (dashboardIndicator, dashboardIndicator) {
	sent := dashboardIndicator{Symbol: "○", Label: "Waiting for Mac", Class: "waiting"}
	if job.Attempts > 0 {
		sent = dashboardIndicator{Symbol: "✓", Label: "Sent to Mac", Class: "success"}
	}

	switch job.State {
	case queue.StateSucceeded:
		return sent, dashboardIndicator{Symbol: "✓", Label: "Confirmed in Things", Class: "success"}
	case queue.StateFailed:
		return sent, dashboardIndicator{Symbol: "✕", Label: "Failed", Class: "failed"}
	case queue.StateLeased:
		if !job.LeaseUntil.IsZero() && !job.LeaseUntil.After(now) {
			return sent, dashboardIndicator{Symbol: "↻", Label: "Lease expired; retry pending", Class: "retrying"}
		}
		return sent, dashboardIndicator{Symbol: "…", Label: "Processing on Mac", Class: "progress"}
	case queue.StateQueued:
		if job.Attempts > 0 {
			return sent, dashboardIndicator{Symbol: "↻", Label: "Retrying", Class: "retrying"}
		}
		return sent, dashboardIndicator{Symbol: "○", Label: "Pending", Class: "waiting"}
	default:
		return sent, dashboardIndicator{Symbol: "?", Label: "Unknown", Class: "failed"}
	}
}

func dashboardDestination(job queue.Job) string {
	if job.Task.Destination == nil || job.Task.Destination.Name == "" {
		return "Inbox"
	}
	switch job.Task.Destination.Kind {
	case "project":
		destination := "Project: " + job.Task.Destination.Name
		if job.Task.Destination.Heading != "" {
			destination += " / " + job.Task.Destination.Heading
		}
		return destination
	case "area":
		return "Area: " + job.Task.Destination.Name
	default:
		return job.Task.Destination.Name
	}
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="10">
  <title>ThingsIndex status</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { max-width: 74rem; margin: 0 auto; padding: 2rem 1rem 4rem; background: Canvas; color: CanvasText; }
    header { display: flex; gap: 1rem; align-items: baseline; justify-content: space-between; flex-wrap: wrap; }
    h1 { font-size: 1.5rem; margin: 0; }
    .updated, .secondary { color: GrayText; font-size: .85rem; }
    .table-wrap { overflow-x: auto; margin-top: 1.5rem; border: 1px solid #8885; border-color: color-mix(in srgb, CanvasText 18%, transparent); border-radius: .75rem; }
    table { width: 100%; border-collapse: collapse; min-width: 44rem; }
    th, td { padding: .85rem 1rem; text-align: left; border-bottom: 1px solid #8884; border-color: color-mix(in srgb, CanvasText 12%, transparent); vertical-align: top; }
    th { font-size: .75rem; text-transform: uppercase; letter-spacing: .06em; color: GrayText; }
    tbody tr:last-child td { border-bottom: 0; }
    .task { min-width: 18rem; }
    .indicator { display: inline-flex; align-items: center; gap: .55rem; white-space: nowrap; }
    .symbol { width: 1.6rem; height: 1.6rem; display: inline-grid; place-items: center; border-radius: 50%; font-weight: 800; }
    .success .symbol { color: #0b6b3a; background: #d9f7e7; }
    .progress .symbol { color: #1d4f91; background: #dcecff; }
    .retrying .symbol { color: #865300; background: #fff0c2; }
    .failed .symbol { color: #9b1c1c; background: #ffe0e0; }
    .waiting .symbol { color: GrayText; background: #8882; background: color-mix(in srgb, CanvasText 8%, transparent); }
    details { margin-top: .45rem; font-size: .85rem; }
    details p, details ul { margin: .4rem 0 0; }
    code { overflow-wrap: anywhere; }
    .empty { padding: 3rem 1rem; text-align: center; color: GrayText; }
    @media (max-width: 42rem) { body { padding-top: 1rem; } .label { font-size: .8rem; } }
  </style>
</head>
<body>
  <header>
    <h1>ThingsIndex status</h1>
    <span class="updated">Updated {{.GeneratedAt}} · refreshes every 10 seconds</span>
  </header>
  {{if .Rows}}
  <div class="table-wrap">
    <table>
      <thead><tr><th>Date</th><th>Task</th><th>Sent to Mac</th><th>Added to Things</th></tr></thead>
      <tbody>
      {{range .Rows}}
        <tr>
          <td><time datetime="{{.CreatedAtISO}}">{{.CreatedAt}}</time></td>
          <td class="task"><strong>{{.Title}}</strong><div class="secondary">{{.Destination}}</div>
            {{if or .LastError .Warnings .ThingsID}}
            <details><summary>Details</summary>
              <p>Attempts: {{.Attempts}}</p>
              {{if .ThingsID}}<p>Things ID: <code>{{.ThingsID}}</code></p>{{end}}
              {{if .LastError}}<p>Error: {{.LastError}}</p>{{end}}
              {{if .Warnings}}<ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>{{end}}
            </details>
            {{end}}
          </td>
          <td><span class="indicator {{.Sent.Class}}"><span class="symbol" aria-hidden="true">{{.Sent.Symbol}}</span><span class="label">{{.Sent.Label}}</span></span></td>
          <td><span class="indicator {{.Confirmed.Class}}"><span class="symbol" aria-hidden="true">{{.Confirmed.Symbol}}</span><span class="label">{{.Confirmed.Label}}</span></span></td>
        </tr>
      {{end}}
      </tbody>
    </table>
  </div>
  {{else}}<div class="empty">No retained capture jobs yet.</div>{{end}}
</body>
</html>`))
