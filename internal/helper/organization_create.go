package helper

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nejmlabs/things-index/internal/capture"
)

// CreateArea creates a named area or reuses its sole exact name match. The
// caller must journal dispatch before calling: AppleScript check/create is not
// a transaction, so an interrupted call must not be blindly dispatched again.
func (c *Client) CreateArea(ctx context.Context, req capture.CreateAreaRequest) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	return c.createOrganizationItem(ctx, "area", req.Title, "")
}

// CreateTag creates a tag with its parent in the same make command. Existing
// tags are reused only if their parent also matches; this never reparents one.
// As with CreateArea, the caller must not replay an uncertain dispatch.
func (c *Client) CreateTag(ctx context.Context, req capture.CreateTagRequest) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	return c.createOrganizationItem(ctx, "tag", req.Title, req.Parent)
}

func (c *Client) createOrganizationItem(ctx context.Context, kind, title, parent string) (Response, error) {
	runner := c.commandRunner()
	_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if errors.Is(runningErr, context.Canceled) || errors.Is(runningErr, context.DeadlineExceeded) {
		return Response{}, runningErr
	}
	if runningErr != nil {
		// AppleScript can implicitly launch Things. Start it hidden first and
		// restore only the app that this call found stopped, including failures.
		defer restoreThingsStoppedState(ctx, runner)
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", "-a", "/Applications/Things3.app"}); err != nil {
			return Response{}, fmt.Errorf("start Things hidden for %s creation: %w", kind, err)
		}
	}
	created, err := runOrganizationScript(ctx, runner, "create", kind, title, parent, "", "")
	if err != nil {
		return Response{}, fmt.Errorf("create Things %s: %w", kind, err)
	}
	// A successful native command alone is insufficient. Read the item back in
	// a separate invocation and check its identity, exact name, uniqueness and
	// resolved parent. No SQLite writes or additional automation surface is used.
	verified, err := runOrganizationScript(ctx, runner, "verify", kind, title, parent, created.id, created.parentID)
	if err != nil {
		return Response{}, fmt.Errorf("verify Things %s: %w", kind, err)
	}
	if verified.id != created.id || verified.parentID != created.parentID {
		return Response{}, &OperationError{Code: "organization_unverified"}
	}
	response := Response{OK: true, ID: verified.id}
	if created.status == "reused" {
		response.Warnings = []string{fmt.Sprintf("ThingsIndex warning: reused existing %s %q.", kind, title)}
	}
	return response, nil
}

type organizationScriptResult struct {
	status   string
	id       string
	parentID string
}

func runOrganizationScript(ctx context.Context, runner CommandRunner, action, kind, title, parent, id, parentID string) (organizationScriptResult, error) {
	// All request strings are argv data, never interpolated into AppleScript.
	stdout, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", organizationScript, "--", action, kind, title, parent, id, parentID})
	if err != nil {
		if ctx.Err() != nil {
			return organizationScriptResult{}, ctx.Err()
		}
		return organizationScriptResult{}, fmt.Errorf("run Things AppleScript: %w", err)
	}
	fields := strings.Split(strings.TrimRight(string(stdout), "\r\n"), "\t")
	if len(fields) == 2 && fields[0] == "error" {
		switch fields[1] {
		case "organization_ambiguous", "parent_not_found", "parent_ambiguous", "parent_conflict", "organization_unverified":
			return organizationScriptResult{}, &OperationError{Code: fields[1]}
		}
	}
	if len(fields) != 3 || !validOrganizationID(fields[1]) || (fields[2] != "" && !validOrganizationID(fields[2])) {
		return organizationScriptResult{}, errors.New("invalid Things organization response")
	}
	if (action == "create" && fields[0] != "created" && fields[0] != "reused") || (action == "verify" && fields[0] != "verified") {
		return organizationScriptResult{}, errors.New("unexpected Things organization result")
	}
	if (kind == "area" || parent == "") && fields[2] != "" || parent != "" && fields[2] == "" {
		return organizationScriptResult{}, &OperationError{Code: "organization_unverified"}
	}
	return organizationScriptResult{status: fields[0], id: fields[1], parentID: fields[2]}, nil
}

func validOrganizationID(value string) bool {
	if value == "" || len(value) > capture.MaxDestinationLen {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

// The public Things dictionary supports area/tag creation and parent tag.
// https://culturedcode.com/things/support/articles/4562654/
// Names are compared in AppleScript rather than by app-defined name lookup;
// only case is ignored. The verification branch cannot execute make.
const organizationScript = `on sameName(leftName, rightName)
    ignoring case
        considering diacriticals, hyphens, punctuation, white space
            return leftName is rightName
        end considering
    end ignoring
end sameName

on matchingItems(itemKind, requestedTitle)
    tell application "Things3"
        if itemKind is "area" then
            set allItems to every area
        else
            set allItems to every tag
        end if
        set matchedItems to {}
        repeat with candidate in allItems
            if my sameName((name of candidate as text), requestedTitle) then
                set end of matchedItems to contents of candidate
            end if
        end repeat
    end tell
    return matchedItems
end matchingItems

on itemParentID(selectedItem)
    tell application "Things3"
        set selectedParent to parent tag of selectedItem
        if selectedParent is missing value then return ""
        return id of selectedParent as text
    end tell
end itemParentID

on run argv
    if (count of argv) is not 6 then error "Invalid organization arguments"
    set operationMode to item 1 of argv
    set itemKind to item 2 of argv
    set requestedTitle to item 3 of argv
    set requestedParent to item 4 of argv
    set expectedID to item 5 of argv
    set expectedParentID to item 6 of argv
    if operationMode is not "create" and operationMode is not "verify" then error "Invalid operation"
    if itemKind is not "area" and itemKind is not "tag" then error "Invalid item kind"

    set parentItem to missing value
    set resolvedParentID to ""
    if itemKind is "tag" and requestedParent is not "" then
        set parents to my matchingItems("tag", requestedParent)
        if (count of parents) is 0 then return "error" & tab & "parent_not_found"
        if (count of parents) is not 1 then return "error" & tab & "parent_ambiguous"
        set parentItem to item 1 of parents
        tell application "Things3" to set resolvedParentID to id of parentItem as text
    end if
    if operationMode is "verify" and resolvedParentID is not expectedParentID then
        return "error" & tab & "organization_unverified"
    end if

    set matches to my matchingItems(itemKind, requestedTitle)
    if (count of matches) is greater than 1 then return "error" & tab & "organization_ambiguous"
    set resultStatus to "reused"
    if (count of matches) is 0 then
        if operationMode is "verify" then return "error" & tab & "organization_unverified"
        tell application "Things3"
            if itemKind is "area" then
                set selectedItem to make new area with properties {name:requestedTitle}
            else if parentItem is missing value then
                set selectedItem to make new tag with properties {name:requestedTitle}
            else
                set selectedItem to make new tag with properties {name:requestedTitle, parent tag:parentItem}
            end if
        end tell
        set resultStatus to "created"
    else
        set selectedItem to item 1 of matches
    end if

    tell application "Things3"
        set selectedID to id of selectedItem as text
        set selectedName to name of selectedItem as text
    end tell
    if not my sameName(selectedName, requestedTitle) then return "error" & tab & "organization_unverified"
    if itemKind is "tag" then
        if my itemParentID(selectedItem) is not resolvedParentID then return "error" & tab & "parent_conflict"
    end if
    if operationMode is "verify" then
        if selectedID is not expectedID then return "error" & tab & "organization_unverified"
        set resultStatus to "verified"
    end if
    return resultStatus & tab & selectedID & tab & resolvedParentID
end run
`
