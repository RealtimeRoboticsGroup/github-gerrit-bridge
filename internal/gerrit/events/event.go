// Package events reads Gerrit's `stream-events` feed.
//
// The bridge only uses events as hints about which change to reconcile, so
// decoding is deliberately lenient: unknown fields are ignored, and a field
// whose JSON shape is not what we expect does not lose the whole event.
package events

import (
	"encoding/json"
	"fmt"
)

// Event is the subset of a stream-events event the bridge reads.
type Event struct {
	Type      string `json:"type"`
	CreatedOn int64  `json:"eventCreatedOn"`

	Change    *Change    `json:"change"`
	PatchSet  *PatchSet  `json:"patchSet"`
	RefUpdate *RefUpdate `json:"refUpdate"`

	// The account that caused the event. Gerrit uses a different key for
	// each event type; see Actor.
	Uploader  *Account `json:"uploader"`
	Author    *Account `json:"author"`
	Submitter *Account `json:"submitter"`
	Abandoner *Account `json:"abandoner"`
	Restorer  *Account `json:"restorer"`
	Changer   *Account `json:"changer"`
	Remover   *Account `json:"remover"`
	Editor    *Account `json:"editor"`
	Reviewer  *Account `json:"reviewer"`

	Comment   string     `json:"comment"`
	Approvals []Approval `json:"approvals"`

	// Raw is the undecoded event, for debug logging.
	Raw json.RawMessage `json:"-"`

	project string
}

// Change is the change attribute of an event.
type Change struct {
	Project string   `json:"project"`
	Branch  string   `json:"branch"`
	Topic   string   `json:"topic"`
	ID      string   `json:"id"` // Change-Id
	Number  int      `json:"number"`
	Subject string   `json:"subject"`
	Owner   *Account `json:"owner"`
	URL     string   `json:"url"`
	Status  string   `json:"status"`
	WIP     bool     `json:"wip"`
	Private bool     `json:"private"`
}

// PatchSet is the patchSet attribute of an event.
type PatchSet struct {
	Number    int      `json:"number"`
	Revision  string   `json:"revision"`
	Parents   []string `json:"parents"`
	Ref       string   `json:"ref"`
	Uploader  *Account `json:"uploader"`
	Author    *Account `json:"author"`
	Kind      string   `json:"kind"`
	CreatedOn int64    `json:"createdOn"`
}

// RefUpdate is the refUpdate attribute of a ref-updated event.
type RefUpdate struct {
	OldRev  string `json:"oldRev"`
	NewRev  string `json:"newRev"`
	RefName string `json:"refName"`
	Project string `json:"project"`
}

// Account identifies a Gerrit user.
type Account struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

// Approval is one label vote. Values are strings in stream-events.
type Approval struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Value       string `json:"value"`
	OldValue    string `json:"oldValue"`
}

// Parse decodes one stream-events line.
//
// Only "type" must decode. If the rest of the event has an unexpected shape
// (a Gerrit upgrade changed a field), the error is returned along with an
// Event holding the type and whatever change number and project could be
// salvaged, so the caller can still schedule a reconcile.
func Parse(line []byte) (*Event, error) {
	var head struct {
		Type    string          `json:"type"`
		Project json.RawMessage `json:"project"`
		Change  struct {
			Number  json.RawMessage `json:"number"`
			Project json.RawMessage `json:"project"`
		} `json:"change"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		// Retry with just the type, in case "change" itself is odd.
		var t struct {
			Type string `json:"type"`
		}
		if err2 := json.Unmarshal(line, &t); err2 != nil || t.Type == "" {
			return nil, fmt.Errorf("not a stream-events event: %w", err)
		}
		head.Type = t.Type
	}
	if head.Type == "" {
		return nil, fmt.Errorf("event has no type")
	}
	e := &Event{}
	err := json.Unmarshal(line, e)
	if err != nil {
		e = &Event{Type: head.Type}
		if n, ok := jsonInt(head.Change.Number); ok {
			e.Change = &Change{Number: n, Project: jsonString(head.Change.Project)}
		}
		err = fmt.Errorf("partially decoded %s event: %w", head.Type, err)
	}
	e.Type = head.Type
	e.Raw = append(json.RawMessage(nil), line...)
	// Top-level "project" is a string in current Gerrit; tolerate others.
	e.project = jsonString(head.Project)
	return e, err
}

// Project is the project the event is about.
func (e *Event) Project() string {
	switch {
	case e.Change != nil && e.Change.Project != "":
		return e.Change.Project
	case e.RefUpdate != nil && e.RefUpdate.Project != "":
		return e.RefUpdate.Project
	}
	return e.project
}

// Actor is the account that caused the event, if Gerrit reported one.
func (e *Event) Actor() *Account {
	for _, a := range []*Account{e.Uploader, e.Submitter, e.Abandoner, e.Restorer,
		e.Changer, e.Remover, e.Editor, e.Author} {
		if a != nil {
			return a
		}
	}
	return nil
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Name
	}
	return ""
}

func jsonInt(raw json.RawMessage) (int, bool) {
	var n int
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return n, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}
