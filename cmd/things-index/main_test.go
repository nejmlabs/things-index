package main

import (
	"strings"
	"testing"
)

func TestBuildLauncherScript(t *testing.T) {
	t.Parallel()

	script := buildLauncherScript(
		"/Users/pat",
		"https://things.example.com",
		"worker-token-00000000000000000000",
		"/Users/pat/Library/Group Containers/things/main.sqlite",
		"thingsToken'; rm -rf /", // hostile paste must stay inside the quotes
		"/usr/local/bin/things-index",
	)

	for _, want := range []string{
		"#!/bin/zsh -l\n",
		"export THINGS_INDEX_SERVER_URL='https://things.example.com'\n",
		"export THINGS_INDEX_WORKER_TOKEN='worker-token-00000000000000000000'\n",
		"export THINGS_INDEX_THINGS_DB_PATH='/Users/pat/Library/Group Containers/things/main.sqlite'\n",
		`export THINGS_INDEX_THINGS_AUTH_TOKEN='thingsToken'\''; rm -rf /'` + "\n",
		"exec '/usr/local/bin/things-index' worker\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("launcher script missing %q:\n%s", want, script)
		}
	}
}

func TestBuildLauncherScriptOmitsAbsentThingsToken(t *testing.T) {
	t.Parallel()

	script := buildLauncherScript("/Users/pat", "http://127.0.0.1:8080", "worker-token-00000000000000000000", "/db.sqlite", "", "/bin/things-index")
	if strings.Contains(script, "THINGS_INDEX_THINGS_AUTH_TOKEN") {
		t.Errorf("launcher script exports an empty Things auth token:\n%s", script)
	}
}

func TestBuildLaunchAgentPlist(t *testing.T) {
	t.Parallel()

	plist := buildLaunchAgentPlist("/Users/pat & co/.local/bin/run-things-worker.sh", "/Users/pat & co/Library/Logs/ThingsIndex")

	for _, want := range []string{
		"<string>" + workerLaunchAgentLabel + "</string>",
		"<string>/Users/pat &amp; co/.local/bin/run-things-worker.sh</string>",
		"<string>/Users/pat &amp; co/Library/Logs/ThingsIndex/worker.log</string>",
		"<string>/Users/pat &amp; co/Library/Logs/ThingsIndex/worker-error.log</string>",
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("LaunchAgent plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "TOKEN") {
		t.Errorf("LaunchAgent plist must not carry secrets:\n%s", plist)
	}
}
