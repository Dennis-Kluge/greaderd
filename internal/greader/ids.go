package greader

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	itemIDPrefix = "tag:google.com,2005:reader/item/"

	streamReadingList = "user/-/state/com.google/reading-list"
	streamStarred     = "user/-/state/com.google/starred"
)

// States that appear as "user/-/state/com.google/<state>" in stream and tag IDs.
const (
	stateReadingList = "reading-list"
	stateRead        = "read"
	stateStarred     = "starred"
	stateKeptUnread  = "kept-unread"
)

func feedStreamID(id int64) string      { return "feed/" + strconv.FormatInt(id, 10) }
func labelStreamID(name string) string  { return "user/-/label/" + name }
func stateStreamID(state string) string { return "user/-/state/com.google/" + state }

// longItemID formats an entry ID in the long hex form. NetNewsWire parses the
// last path segment of the item IDs in stream contents as hexadecimal.
func longItemID(id int64) string {
	return fmt.Sprintf("%s%016x", itemIDPrefix, uint64(id))
}

// parseItemID accepts the long hex form and the short decimal form, as
// FreshRSS does. NetNewsWire sends the long form.
func parseItemID(s string) (int64, error) {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		u, err := strconv.ParseUint(s[i+1:], 16, 64)
		return int64(u), err
	}
	return strconv.ParseInt(s, 10, 64)
}

func parseItemIDs(values []string) ([]int64, error) {
	ids := make([]int64, 0, len(values))
	for _, v := range values {
		id, err := parseItemID(v)
		if err != nil {
			return nil, fmt.Errorf("invalid item id %q", v)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// parseFeedStream returns the reference (ID or URL) of a "feed/<ref>" stream.
func parseFeedStream(s string) (string, bool) {
	ref, ok := strings.CutPrefix(s, "feed/")
	if !ok || ref == "" {
		return "", false
	}
	return ref, true
}

// parseLabel returns the folder name of a "user/<user>/label/<name>" stream.
// Clients use "-" or the numeric user ID for <user>.
func parseLabel(s string) (string, bool) {
	name, ok := strings.CutPrefix(userPath(s), "label/")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// parseState returns <state> of "user/<user>/state/com.google/<state>", or ""
// for any other stream ID.
func parseState(s string) string {
	state, ok := strings.CutPrefix(userPath(s), "state/com.google/")
	if !ok {
		return ""
	}
	return state
}

// userPath strips "user/<user>/" from s; it returns "" if s has no such prefix.
func userPath(s string) string {
	rest, ok := strings.CutPrefix(s, "user/")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return rest
}
