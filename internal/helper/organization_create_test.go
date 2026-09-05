package helper

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
)

type organizationRunner struct {
	t     *testing.T
	calls [][]string
	run   func(context.Context, []string) ([]byte, error)
}

func (r *organizationRunner) Run(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	r.t.Helper()
	if executable == "/usr/bin/pgrep" && reflect.DeepEqual(args, []string{"-x", "Things3"}) {
		return nil, nil, nil // Existing focused protocol tests keep Things running.
	}
	if executable != "/usr/bin/osascript" || len(args) != 9 || args[0] != "-e" || args[1] != organizationScript || args[2] != "--" {
		r.t.Fatalf("unexpected native invocation: %s %q", executable, args)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		r.t.Fatal("native call has no deadline")
	}
	r.calls = append(r.calls, append([]string(nil), args[3:]...))
	if r.run != nil {
		stdout, err := r.run(ctx, args[3:])
		return stdout, []byte("private native diagnostics"), err
	}
	r.t.Fatal("unexpected native execution")
	return nil, nil, nil
}

func TestCreateAreaVerifiesNativeIdentityAndPassesNamesAsData(t *testing.T) {
	t.Parallel()
	title := `Home " & do shell script "touch /tmp/injection" -- \\`
	runner := &organizationRunner{t: t}
	runner.run = func(_ context.Context, args []string) ([]byte, error) {
		if strings.Contains(organizationScript, title) {
			t.Fatal("request text was interpolated into AppleScript")
		}
		if args[2] != title {
			t.Fatalf("title changed: %q", args[2])
		}
		if args[0] == "create" {
			return []byte("created\tarea-uuid\t\n"), nil
		}
		return []byte("verified\tarea-uuid\t\n"), nil
	}
	client := &Client{Runner: runner}
	response, err := client.CreateArea(context.Background(), capture.CreateAreaRequest{Title: title})
	if err != nil || !response.OK || response.ID != "area-uuid" || len(response.Warnings) != 0 {
		t.Fatalf("response=%+v, error=%v", response, err)
	}
	want := [][]string{{"create", "area", title, "", "", ""}, {"verify", "area", title, "", "area-uuid", ""}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls=%q, want %q", runner.calls, want)
	}
}

func TestCreateTagReusesOnlyVerifiedParentAndReportsWarning(t *testing.T) {
	t.Parallel()
	runner := &organizationRunner{t: t}
	runner.run = func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "create" {
			return []byte("reused\ttag-uuid\tparent-uuid\n"), nil
		}
		return []byte("verified\ttag-uuid\tparent-uuid\n"), nil
	}
	client := &Client{Runner: runner}
	response, err := client.CreateTag(context.Background(), capture.CreateTagRequest{Title: "Garden", Parent: "Home"})
	if err != nil || !response.OK || response.ID != "tag-uuid" || len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "reused existing tag") {
		t.Fatalf("response=%+v, error=%v", response, err)
	}
	want := [][]string{{"create", "tag", "Garden", "Home", "", ""}, {"verify", "tag", "Garden", "Home", "tag-uuid", "parent-uuid"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls=%q, want %q", runner.calls, want)
	}
}

func TestCreateOrganizationRejectsAmbiguityAndParentErrorsWithoutFurtherDispatch(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"organization_ambiguous", "parent_not_found", "parent_ambiguous", "parent_conflict"} {
		t.Run(code, func(t *testing.T) {
			runner := &organizationRunner{t: t, run: func(context.Context, []string) ([]byte, error) {
				return []byte("error\t" + code + "\n"), nil
			}}
			client := &Client{Runner: runner}
			_, err := client.CreateTag(context.Background(), capture.CreateTagRequest{Title: "Garden", Parent: "Home"})
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != code || len(runner.calls) != 1 {
				t.Fatalf("error=%v, calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestCreateOrganizationNeverReportsUnverifiedResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		create string
		verify string
	}{
		{"missing id", "created\t\t", ""},
		{"unsafe id", "created\tid\ninjection\t", ""},
		{"unknown status", "success\tarea-id\t", ""},
		{"unknown error", "error\tnative_secret", ""},
		{"extra output", "created\tarea-id\t\textra", ""},
		{"unexpected parent", "created\tarea-id\tparent-id", ""},
		{"missing readback", "created\tarea-id\t", "error\torganization_unverified"},
		{"different identity", "created\tarea-id\t", "verified\tother-id\t"},
		{"unexpected readback status", "created\tarea-id\t", "created\tarea-id\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &organizationRunner{t: t, run: func(_ context.Context, args []string) ([]byte, error) {
				if args[0] == "create" {
					return []byte(tc.create + "\n"), nil
				}
				return []byte(tc.verify + "\n"), nil
			}}
			client := &Client{Runner: runner}
			response, err := client.CreateArea(context.Background(), capture.CreateAreaRequest{Title: "Home"})
			if err == nil || response.OK || response.ID != "" || strings.Contains(err.Error(), "native_secret") {
				t.Fatalf("response=%+v, error=%v", response, err)
			}
		})
	}
}

func TestCreateTagRejectsChangedParentDuringReadback(t *testing.T) {
	t.Parallel()
	runner := &organizationRunner{t: t, run: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "create" {
			return []byte("created\ttag-id\tparent-one\n"), nil
		}
		return []byte("verified\ttag-id\tparent-two\n"), nil
	}}
	client := &Client{Runner: runner}
	response, err := client.CreateTag(context.Background(), capture.CreateTagRequest{Title: "Garden", Parent: "Home"})
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "organization_unverified" || response.OK {
		t.Fatalf("response=%+v, error=%v", response, err)
	}
}

func TestCreateOrganizationValidatesBeforeNativeDispatch(t *testing.T) {
	t.Parallel()
	runner := &organizationRunner{t: t}
	client := &Client{Runner: runner}
	if _, err := client.CreateArea(context.Background(), capture.CreateAreaRequest{}); err == nil {
		t.Fatal("accepted missing area name")
	}
	if _, err := client.CreateTag(context.Background(), capture.CreateTagRequest{Title: "Home", Parent: "home"}); err == nil {
		t.Fatal("accepted self-parenting tag")
	}
	if len(runner.calls) != 0 {
		t.Fatal("invalid request reached native execution")
	}
}

func TestCreateOrganizationDoesNotReplayFailedNativeDispatch(t *testing.T) {
	t.Parallel()
	nativeErr := errors.New("command failed")
	runner := &organizationRunner{t: t, run: func(context.Context, []string) ([]byte, error) {
		return nil, nativeErr
	}}
	client := &Client{Runner: runner}
	_, err := client.CreateArea(context.Background(), capture.CreateAreaRequest{Title: "Home"})
	if !errors.Is(err, nativeErr) || len(runner.calls) != 1 || strings.Contains(err.Error(), "private native diagnostics") {
		t.Fatalf("error=%v, calls=%d", err, len(runner.calls))
	}
}

func TestOrganizationScriptKeepsVerificationReadOnlyAndParentOnCreate(t *testing.T) {
	t.Parallel()
	// These are contract checks, not a claim that a mock executes AppleScript.
	// Live acceptance of the new branches is deliberately separate.
	for _, required := range []string{
		`if operationMode is "verify" then return "error" & tab & "organization_unverified"`,
		`make new tag with properties {name:requestedTitle, parent tag:parentItem}`,
		`if selectedID is not expectedID then`,
		`resolvedParentID is not expectedParentID`,
		`if my itemParentID(selectedItem) is not resolvedParentID`,
		`considering diacriticals, hyphens, punctuation, white space`,
	} {
		if !strings.Contains(organizationScript, required) {
			t.Fatalf("missing native contract: %s", required)
		}
	}
	for _, forbidden := range []string{"set parent tag", "activate", "do shell script", "display dialog", "choose from list"} {
		if strings.Contains(organizationScript, forbidden) {
			t.Fatalf("unexpected native action: %s", forbidden)
		}
	}
}

func TestCreateOrganizationRestoresOriginallyStoppedApp(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"", "launch", "create", "verify", "cancel"} {
		t.Run("failure="+failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			pgrepCalls := 0
			runner := captureContextRunner(func(callCtx context.Context, executable string, args []string) ([]byte, []byte, error) {
				if executable == "/usr/bin/pgrep" {
					pgrepCalls++
					calls = append(calls, "pgrep")
					if pgrepCalls == 1 {
						return nil, nil, errors.New("not running")
					}
					return nil, nil, nil
				}
				if executable == "/usr/bin/open" {
					calls = append(calls, "open-hidden")
					if !reflect.DeepEqual(args, []string{"-g", "-j", "-a", "/Applications/Things3.app"}) {
						t.Fatalf("unexpected launch: %q", args)
					}
					if failure == "launch" {
						return nil, nil, errors.New("launch failed after starting Things")
					}
					return nil, nil, nil
				}
				if executable != "/usr/bin/osascript" {
					t.Fatalf("unexpected command: %s", executable)
				}
				if len(args) == 2 && strings.Contains(args[1], "to quit") {
					calls = append(calls, "quit")
					if callCtx.Err() != nil {
						t.Fatal("cleanup inherited canceled context")
					}
					if _, bounded := callCtx.Deadline(); !bounded {
						t.Fatal("cleanup has no deadline")
					}
					return nil, nil, nil
				}
				if len(args) != 9 || args[1] != organizationScript {
					t.Fatalf("unexpected script: %q", args)
				}
				operation := args[3]
				calls = append(calls, operation)
				if failure == operation {
					return nil, nil, errors.New("native script failed")
				}
				if failure == "cancel" {
					cancel()
					return nil, nil, context.Canceled
				}
				if operation == "create" {
					return []byte("created\tarea-id\t\n"), nil, nil
				}
				return []byte("verified\tarea-id\t\n"), nil, nil
			})
			client := &Client{Runner: runner}
			response, err := client.CreateArea(ctx, capture.CreateAreaRequest{Title: "Home"})
			if (err == nil) != (failure == "") || response.OK != (failure == "") {
				t.Fatalf("response=%+v, error=%v", response, err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			want := []string{"pgrep", "open-hidden"}
			if failure != "launch" {
				want = append(want, "create")
				if failure != "create" && failure != "cancel" {
					want = append(want, "verify")
				}
			}
			want = append(want, "pgrep", "quit")
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%q, want %q", calls, want)
			}
		})
	}
}

func TestCreateOrganizationStopsBeforeLaunchWhenRunningCheckCanceled(t *testing.T) {
	t.Parallel()
	client := &Client{Runner: captureContextRunner(func(_ context.Context, executable string, _ []string) ([]byte, []byte, error) {
		if executable != "/usr/bin/pgrep" {
			t.Fatal("canceled running check launched Things")
		}
		return nil, nil, context.DeadlineExceeded
	})}
	_, err := client.CreateTag(context.Background(), capture.CreateTagRequest{Title: "Home"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running-check deadline lost: %v", err)
	}
}
